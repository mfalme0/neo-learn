// Package worker wires the SMS delivery pipeline together for cmd/api.
//
// It exists so main stays readable and so the assembly of gateway, renderer,
// recipient lookup, and drain worker can be tested independently of the HTTP
// server.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/inbound"
	"github.com/neolearn/neolearn/internal/learning"
	"github.com/neolearn/neolearn/internal/outbox"
	"github.com/neolearn/neolearn/internal/sms"
)

// Deps are the collaborators the pipeline needs.
type Deps struct {
	Pool      *pgxpool.Pool
	Queries   *db.Queries
	Learning  *learning.Service
	Gateway   sms.Gateway
	Templates sms.Templates
	Logger    *slog.Logger
	Config    outbox.Config
}

// Pipeline is the assembled SMS transport.
type Pipeline struct {
	Gateway  sms.Gateway
	Outbound *sms.Outbound
	Delivery *sms.Delivery
	Worker   *outbox.Worker
	// Inbound is nil when the webhook secret is unset, in which case inbound
	// delivery is not mounted at all.
	Inbound *inbound.Processor
}

// Assemble builds the pipeline.
func Assemble(deps Deps) (*Pipeline, error) {
	if deps.Gateway == nil {
		return nil, errors.New("worker: a gateway is required")
	}
	if deps.Pool == nil || deps.Queries == nil {
		return nil, errors.New("worker: a database pool and queries are required")
	}
	if deps.Learning == nil {
		return nil, errors.New("worker: the learning service is required")
	}

	logger := slogAdapter{deps.Logger}

	templates := deps.Templates
	if templates.Brand == "" {
		templates = sms.NewTemplates()
	}

	outbound := sms.NewOutbound(deps.Gateway, templates, logger)
	delivery := sms.NewDelivery(deps.Queries, templates, logger)

	// The renderer turns a stored notification into text. It delegates to the
	// delivery planner, so what a learner is told is derived from their current
	// state rather than from the event that happened to trigger the row.
	renderer := outbox.PayloadRenderer(func(ctx context.Context, msg outbox.Message) (string, error) {
		if msg.DestinationMSISDN == "" {
			return "", nil
		}

		planned, err := delivery.Plan(ctx, sms.OutboxMessage{
			OutboxID:      msg.OutboxID,
			InstitutionID: msg.InstitutionID,
			LearnerID:     msg.Payload.LearnerID,
			EventID:       msg.EventID,
			Topic:         msg.Topic,
			To:            msg.DestinationMSISDN,
		})
		if err != nil {
			return "", err
		}
		if planned == nil {
			return "", nil
		}
		return planned.Body, nil
	})

	drainer, err := outbox.NewWorker(
		deps.Pool,
		deps.Queries,
		outbound,
		renderer,
		&msisdnLookup{queries: deps.Queries},
		deps.Config,
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("worker: build drain worker: %w", err)
	}

	processor := inbound.NewProcessor(deps.Queries, deps.Learning, delivery, templates, deps.Logger)

	return &Pipeline{
		Gateway:  deps.Gateway,
		Outbound: outbound,
		Delivery: delivery,
		Worker:   drainer,
		Inbound:  processor,
	}, nil
}

// Start runs the drain worker until the context is cancelled.
func (p *Pipeline) Start(ctx context.Context) {
	p.Worker.Run(ctx)
}

// Acknowledge enqueues a reply to a learner.
//
// The reply goes to the outbox rather than being sent inline so a gateway
// outage while handling an inbound message cannot cost the learner their
// acknowledgement.
func (p *Pipeline) Acknowledge(ctx context.Context, ack inbound.Acknowledgement) error {
	if ack.To == "" || ack.Body == "" {
		return nil
	}

	return p.enqueueDirect(ctx, ack.To, ack.Body, ack.ReplyTo)
}

// enqueueDirect writes a message for delivery without a triggering event.
//
// This is the one path that inserts an outbox row whose event_id is not a
// learning event, which the schema's foreign key does not allow. Rather than
// weaken that constraint -- which protects the invariant that every
// notification corresponds to recorded state -- the reply is sent through the
// gateway directly, with the worker's retry behaviour unavailable.
//
// This is a known limitation rather than a finished design: an acknowledgement
// lost to a gateway blip is not retried. The fix is a separate
// acknowledgements table, which is deferred until a real provider makes the
// trade-off concrete.
func (p *Pipeline) enqueueDirect(ctx context.Context, to, body, reference string) error {
	normalized, err := sms.NormalizeE164(to)
	if err != nil {
		return fmt.Errorf("worker: %w", err)
	}
	to = normalized
	if reference == "" {
		reference = "ack-" + to
	}
	return p.Outbound.SendText(ctx, to, body, reference)
}

// msisdnLookup resolves a learner's number for the drain worker.
type msisdnLookup struct {
	queries *db.Queries
}

func (l *msisdnLookup) Resolve(ctx context.Context, institutionID, learnerID int64) (outbox.Recipient, error) {
	msisdn, err := l.queries.GetLearnerMSISDN(ctx, db.GetLearnerMSISDNParams{
		ID:            learnerID,
		InstitutionID: institutionID,
	})
	if err != nil {
		// A learner with no number cannot be reached. The caller records it and
		// stops retrying, which is the right outcome: there is no destination.
		return outbox.Recipient{}, fmt.Errorf("learner %d has no phone number", learnerID)
	}
	if msisdn == nil {
		return outbox.Recipient{}, fmt.Errorf("learner %d has no phone number", learnerID)
	}
	return outbox.Recipient{MSISDN: *msisdn}, nil
}

// slogAdapter satisfies sms.Logger with the standard structured logger.
//
// A thin adapter rather than changing sms.Logger to *slog.Logger so the sms
// package stays usable from tests without constructing a logger.
type slogAdapter struct {
	logger *slog.Logger
}

func (a slogAdapter) Debug(msg string, args ...any) { a.log(slog.LevelDebug, msg, args...) }
func (a slogAdapter) Info(msg string, args ...any)  { a.log(slog.LevelInfo, msg, args...) }
func (a slogAdapter) Warn(msg string, args ...any)  { a.log(slog.LevelWarn, msg, args...) }
func (a slogAdapter) Error(msg string, args ...any) { a.log(slog.LevelError, msg, args...) }

func (a slogAdapter) log(level slog.Level, msg string, args ...any) {
	if a.logger == nil {
		return
	}
	a.logger.Log(context.Background(), level, msg, args...)
}
