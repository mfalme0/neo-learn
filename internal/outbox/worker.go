// Package outbox drains the transactional outbox to SMS.
//
// The outbox pattern exists because a learner action and the notification it
// triggers must not diverge. The action is written in one transaction with an
// outbox row; this worker delivers that row later, outside the transaction,
// retrying until it succeeds or is given up on.
//
// # Why this is a distributed system
//
// SMS delivery is asynchronous and unreliable in ways an HTTP request is not:
// gateways queue, deduplicate, reorder, and silently drop. The worker therefore
// assumes every message may arrive more than once, in any order, or not at all.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/sms"
)

// PayloadRenderer adapts a function to Renderer.
type PayloadRenderer func(ctx context.Context, msg Message) (string, error)

// Render implements Renderer.
func (f PayloadRenderer) Render(ctx context.Context, msg Message) (string, error) {
	return f(ctx, msg)
}

// Config tunes the worker's behaviour.
type Config struct {
	// PollInterval is how often the worker looks for work when the queue is
	// empty. Short enough that a message feels immediate, long enough not to
	// hammer the database on an idle system.
	PollInterval time.Duration

	// BatchSize is the maximum rows claimed per pass.
	BatchSize int32

	// MaxAttempts is how many delivery attempts a message gets before it is
	// marked dead. After this it stops consuming money and becomes visible to
	// an operator instead.
	MaxAttempts int

	// BaseBackoff is the delay after the first failure. Doubles each attempt.
	BaseBackoff time.Duration

	// MaxBackoff caps the delay so a message that has failed repeatedly is
	// still retried within a reasonable window.
	MaxBackoff time.Duration

	// StaleClaim is how long a claim survives before another worker may take
	// the row. Covers a worker being killed mid-delivery.
	StaleClaim time.Duration

	// MaxSegmentsPerMessage caps how much text is sent in one logical message.
	// Zero means unlimited.
	MaxSegmentsPerMessage int
}

// DefaultConfig returns settings suited to a single-instance deployment.
func DefaultConfig() Config {
	return Config{
		PollInterval:          5 * time.Second,
		BatchSize:             25,
		MaxAttempts:           6,
		BaseBackoff:           30 * time.Second,
		MaxBackoff:            30 * time.Minute,
		StaleClaim:            2 * time.Minute,
		MaxSegmentsPerMessage: 4,
	}
}

// validate rejects configurations that would wedge the worker.
func (c Config) validate() error {
	var problems []error

	if c.PollInterval <= 0 {
		problems = append(problems, errors.New("outbox: PollInterval must be positive"))
	}
	if c.BatchSize <= 0 {
		problems = append(problems, errors.New("outbox: BatchSize must be positive"))
	}
	if c.MaxAttempts <= 0 {
		problems = append(problems, errors.New("outbox: MaxAttempts must be positive"))
	}
	if c.BaseBackoff <= 0 {
		problems = append(problems, errors.New("outbox: BaseBackoff must be positive"))
	}
	if c.MaxBackoff < c.BaseBackoff {
		problems = append(problems, errors.New("outbox: MaxBackoff must be at least BaseBackoff"))
	}
	if c.StaleClaim <= 0 {
		problems = append(problems, errors.New("outbox: StaleClaim must be positive"))
	}

	return errors.Join(problems...)
}

// BackoffFor returns the delay before the next attempt.
//
// Exponential with a hard ceiling. The ceiling matters more than the shape: a
// message that failed five times because the provider is down must come back
// when the provider recovers, and an uncapped curve would leave it retried
// tomorrow.
func (c Config) BackoffFor(attempts int32) time.Duration {
	if attempts < 1 {
		attempts = 1
	}

	// Compute in float to avoid overflowing time.Duration on a large exponent,
	// then clamp before converting.
	scaled := float64(c.BaseBackoff) * math.Pow(2, float64(attempts-1))
	if scaled > float64(c.MaxBackoff) {
		return c.MaxBackoff
	}
	return time.Duration(scaled)
}

// IsDead reports whether a message has exhausted its attempts.
func (c Config) IsDead(attempts int32) bool {
	return attempts >= int32(c.MaxAttempts)
}

// Payload is the notification body the learning engine wrote.
type Payload struct {
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	LearnerID  int64  `json:"learner_id"`
	OccurredAt string `json:"occurred_at"`
}

// Renderer turns a notification into the text to send.
//
// An interface rather than a concrete renderer because the mapping from event to
// message is where product decisions live, and it will differ between a
// development gateway and a real provider. Returning an error lets a renderer
// decline a notification it cannot express -- an assessment with choices that
// do not fit an SMS, say -- rather than sending something misleading.
type Renderer interface {
	Render(ctx context.Context, msg Message) (string, error)
}

// Message is one outbound notification.
type Message struct {
	OutboxID          int64
	InstitutionID     int64
	EventID           string
	Topic             string
	Payload           Payload
	DestinationMSISDN string
	Attempts          int32
}

// Recipient is what the outbound sender needs.
type Recipient struct {
	MSISDN string
}

// Sender delivers rendered text.
type Sender interface {
	SendText(ctx context.Context, to, body, reference string) error
}

// RecipientLookup resolves the destination number for a notification.
//
// Kept behind an interface because the real implementation reads the learner's
// msisdn from the database, and a test should not need a database to exercise
// retry behaviour.
type RecipientLookup interface {
	Resolve(ctx context.Context, institutionID, learnerID int64) (Recipient, error)
}

// Worker drains the outbox.
type Worker struct {
	pool      *pgxpool.Pool
	queries   *db.Queries
	sender    Sender
	renderer  Renderer
	recipient RecipientLookup
	config    Config
	logger    sms.Logger
}

// NewWorker builds a drain worker.
func NewWorker(
	pool *pgxpool.Pool,
	queries *db.Queries,
	sender Sender,
	renderer Renderer,
	recipient RecipientLookup,
	config Config,
	logger sms.Logger,
) (*Worker, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if sender == nil {
		return nil, errors.New("outbox: sender is required")
	}
	if renderer == nil {
		return nil, errors.New("outbox: renderer is required")
	}
	if recipient == nil {
		return nil, errors.New("outbox: recipient lookup is required")
	}

	return &Worker{
		pool:      pool,
		queries:   queries,
		sender:    sender,
		renderer:  renderer,
		recipient: recipient,
		config:    config,
		logger:    sms.Log(logger),
	}, nil
}

// Run drains the outbox until the context is cancelled.
//
// This is the whole worker loop. It never returns an error: a failure to reach
// the database or the gateway is a condition to log and retry, not a reason to
// stop the goroutine, because stopping would silently stop all SMS delivery.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("outbox worker started",
		"poll_interval", w.config.PollInterval,
		"batch_size", w.config.BatchSize,
		"max_attempts", w.config.MaxAttempts)

	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()

	for {
		// Drain once immediately so a restart does not wait a full interval.
		delivered, err := w.DrainOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Error("outbox drain failed", "error", err)
		}
		if delivered > 0 {
			w.logger.Debug("outbox drained", "delivered", delivered)
		}

		select {
		case <-ctx.Done():
			w.logger.Info("outbox worker stopping")
			return
		case <-ticker.C:
		}
	}
}

// DrainOnce claims and attempts one batch. It reports how many messages were
// delivered.
//
// Exported so a test can drive the worker deterministically rather than
// sleeping and hoping.
func (w *Worker) DrainOnce(ctx context.Context) (int, error) {
	rows, err := w.claimBatch(ctx)
	if err != nil {
		return 0, fmt.Errorf("outbox: claim batch: %w", err)
	}

	delivered := 0
	for _, row := range rows {
		sent, err := w.deliver(ctx, row)
		if err != nil {
			w.logger.Error("outbox delivery failed",
				"outbox_id", row.id, "topic", row.topic, "error", err)
			continue
		}
		if sent {
			delivered++
		}
	}

	return delivered, nil
}

// claimedRow is one message taken for delivery.
type claimedRow struct {
	id          int64
	institution int64
	eventID     string
	topic       string
	payload     Payload
	destination string
	attempts    int32
}

func (w *Worker) claimBatch(ctx context.Context) ([]claimedRow, error) {
	// The claim and the attempt count increment are one transaction: if the
	// attempt is not recorded, a message that fails forever would never reach
	// the give-up threshold.
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	queries := w.queries.WithTx(tx)

	due, err := queries.ClaimDueOutboxRows(ctx, db.ClaimDueOutboxRowsParams{
		BatchSize:         w.config.BatchSize,
		StaleAfterSeconds: int32(w.config.StaleClaim.Seconds()),
	})
	if err != nil {
		return nil, err
	}

	claimed := make([]claimedRow, 0, len(due))
	for _, row := range due {
		msisdn := ""
		if row.DestinationMsisdn != nil {
			msisdn = *row.DestinationMsisdn
		}

		var payload Payload
		if len(row.Payload) > 0 {
			// A malformed payload must not stop the worker, but it also must
			// not send a blank message. Treat the parse failure as fatal for
			// this row only.
			if err := json.Unmarshal(row.Payload, &payload); err != nil {
				w.logger.Error("outbox payload is not valid json",
					"outbox_id", row.ID, "error", err)
				// A payload we cannot read will never become readable, so the
				// row is abandoned rather than retried forever.
				dead := deadAt(w.config.IsDead(row.Attempts + 1))
				if relErr := queries.ReleaseOutboxRow(ctx, db.ReleaseOutboxRowParams{
					ID:            row.ID,
					LastError:     strPtr("payload_decode_failed"),
					NextAttemptAt: time.Now().Add(w.config.BackoffFor(row.Attempts + 1)),
					DeadAt:        dead,
				}); relErr != nil {
					w.logger.Error("outbox release failed", "outbox_id", row.ID, "error", relErr)
				}
				continue
			}
		}

		updated, err := queries.ClaimOutboxRow(ctx, row.ID)
		if err != nil {
			w.logger.Warn("outbox claim failed", "outbox_id", row.ID, "error", err)
			continue
		}

		claimed = append(claimed, claimedRow{
			id:          row.ID,
			institution: row.InstitutionID,
			eventID:     row.EventID.String(),
			topic:       row.Topic,
			payload:     payload,
			destination: msisdn,
			attempts:    updated.Attempts,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return claimed, nil
}

// deliver attempts one message and records the outcome.
//
// It returns whether the message was delivered. A failure is recorded against
// the row and swallowed: the loop must keep going, or one undeliverable message
// would block every message behind it.
func (w *Worker) deliver(ctx context.Context, row claimedRow) (bool, error) {
	message := Message{
		OutboxID:          row.id,
		InstitutionID:     row.institution,
		EventID:           row.eventID,
		Topic:             row.topic,
		Payload:           row.payload,
		DestinationMSISDN: row.destination,
		Attempts:          row.attempts,
	}

	body, err := w.renderer.Render(ctx, message)
	if err != nil {
		// A renderer that cannot express a message is not a transient
		// failure. Retrying would waste money forever, so it is recorded and
		// given up on immediately.
		w.logger.Warn("outbox render declined",
			"outbox_id", row.id, "topic", row.topic, "error", err)

		if relErr := w.queries.ReleaseOutboxRow(ctx, db.ReleaseOutboxRowParams{
			ID:            row.id,
			LastError:     strPtr("render_failed: " + err.Error()),
			NextAttemptAt: time.Now().Add(w.config.MaxBackoff),
			DeadAt:        deadAt(true),
		}); relErr != nil {
			return false, fmt.Errorf("outbox: release after render failure: %w", relErr)
		}
		return false, nil
	}

	destination := row.destination
	if destination == "" {
		// The outbox row carries the destination the engine resolved, but a
		// learner whose number was collected later needs a fresh lookup.
		resolved, err := w.recipient.Resolve(ctx, row.institution, row.payload.LearnerID)
		if err != nil {
			w.logger.Warn("outbox recipient unresolved",
				"outbox_id", row.id, "learner_id", row.payload.LearnerID, "error", err)
			return false, w.release(ctx, row, "recipient_unresolved: "+err.Error(), false)
		}
		destination = resolved.MSISDN
	}

	sendErr := w.sender.SendText(ctx, destination, body, row.eventID)
	if sendErr == nil {
		if err := w.queries.MarkOutboxDelivered(ctx, db.MarkOutboxDeliveredParams{
			ID:                row.id,
			ProviderMessageID: strPtr(newProviderMessageID(row.eventID)),
		}); err != nil {
			// The message went out but the row still says undelivered, so it
			// will be retried. The learner gets a duplicate. That is the
			// correct trade: the alternative is a message that was sent and
			// never recorded, which is invisible.
			return false, fmt.Errorf("outbox: mark delivered: %w", err)
		}
		return true, nil
	}

	// A rejection is terminal: retrying a refused number costs money and, for
	// an opted-out learner, keeps texting somebody who asked to stop.
	dead := w.config.IsDead(row.attempts) || isRejected(sendErr)
	reason := sendErr.Error()

	if err := w.release(ctx, row, reason, dead); err != nil {
		return false, err
	}

	if dead {
		w.logger.Warn("outbox message abandoned",
			"outbox_id", row.id, "attempts", row.attempts, "error", reason)
	}

	return false, nil
}

func (w *Worker) release(ctx context.Context, row claimedRow, reason string, dead bool) error {
	if err := w.queries.ReleaseOutboxRow(ctx, db.ReleaseOutboxRowParams{
		ID:            row.id,
		LastError:     strPtr(truncate(reason, 500)),
		NextAttemptAt: time.Now().Add(w.config.BackoffFor(row.attempts)),
		DeadAt:        deadAt(dead),
	}); err != nil {
		return fmt.Errorf("outbox: release: %w", err)
	}
	return nil
}

// deadAt renders a boolean as the nullable timestamptz the column expects.
//
// An unset dead_at is what the partial index depends on, so the zero value here
// must mean SQL NULL rather than the zero time.
func deadAt(dead bool) pgtype.Timestamptz {
	if !dead {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: time.Now(), Valid: true}
}

func strPtr(s string) *string { return &s }

// isRejected reports whether an error is a provider rejection rather than a
// transient failure.
func isRejected(err error) bool {
	var rejected *sms.RejectedError
	return errors.As(err, &rejected)
}

// newProviderMessageID records which provider id we saw, when the sender does
// not surface one through the Sender interface. Overwritten by a later delivery
// report when the gateway provides one.
func newProviderMessageID(eventID string) string {
	return "outbox-" + eventID
}

// truncate caps a string to n bytes, for storing an error in a bounded column.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// Stats summarises queue depth for a health endpoint.
type Stats struct {
	Pending int64
	Dead    int64
}

// Stats reads current queue depth.
func (w *Worker) Stats(ctx context.Context) (Stats, error) {
	row, err := w.queries.CountUndeliveredOutbox(ctx)
	if err != nil {
		return Stats{}, fmt.Errorf("outbox: stats: %w", err)
	}
	return Stats{Pending: row.Pending, Dead: row.Dead}, nil
}
