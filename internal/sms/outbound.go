package sms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoGateway is returned when an operation needs a gateway but none is
// configured. Distinct from a gateway that is configured and failing.
var ErrNoGateway = errors.New("sms: no gateway configured")

// Logger is the minimal logging surface this package needs.
//
// An interface rather than a concrete slog.Logger so the package does not force
// a logging choice on a caller, and so tests can capture output.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// NopLogger discards everything.
//
// Callers may legitimately pass nil for logging -- a test, or a deployment that
// does not want worker output -- so consumers of Logger must tolerate a nil
// value. This exists so they can do so in one call each rather than guarding
// every call site.
var NopLogger Logger = nopLogger{}

// Log returns l, or a discarding logger when l is nil.
func Log(l Logger) Logger {
	if l == nil {
		return NopLogger
	}
	return l
}

type nopLogger struct{}

func (nopLogger) Debug(string, ...any) {}
func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// LogSender is a Gateway that records what it was asked to send and can be made
// to fail on demand.
//
// This exists because the alternative during development is either paying for
// real SMS or wiring a fake into every call site. It is a real implementation of
// the interface, not a test double: the outbox worker, the scheduler, and the
// inbound path all run against it unchanged, so a mis-shaped message is
// discovered before a provider is ever contacted.
type LogSender struct {
	// Sent receives every message the platform attempted. Consuming it is how
	// a development session inspects what a learner would have received.
	Sent chan Message

	// FailNext causes the next N sends to fail with ErrGatewayUnavailable.
	// Used to exercise the retry path without waiting for a real outage.
	FailNext int

	// Delay simulates provider latency.
	Delay time.Duration

	name string
}

// NewLogSender builds a logging gateway.
//
// The channel is buffered so a send that nobody is watching does not block the
// worker. Overflow drops the message rather than stalling delivery.
func NewLogSender(buffer int) *LogSender {
	if buffer <= 0 {
		buffer = 256
	}
	return &LogSender{Sent: make(chan Message, buffer), name: "log"}
}

// Name implements Gateway.
func (l *LogSender) Name() string { return l.name }

// Capabilities implements capabilitiesReporter.
//
// Reports no inbound support: this gateway receives nothing, so the scheduler
// must not rely on a reply arriving. That is the honest description and it keeps
// the one-way limitation explicit.
func (l *LogSender) Capabilities() Capabilities {
	return Capabilities{Inbound: false}
}

// Send implements Gateway.
func (l *LogSender) Send(ctx context.Context, msg Message) (Receipt, error) {
	if l.Delay > 0 {
		select {
		case <-time.After(l.Delay):
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		}
	}

	if l.FailNext > 0 {
		l.FailNext--
		return Receipt{Status: StatusFailed, Error: "simulated gateway outage"}, ErrGatewayUnavailable
	}

	// Non-blocking: a full channel means nobody is listening, which is not a
	// reason to fail a delivery.
	select {
	case l.Sent <- msg:
	default:
	}

	return Receipt{
		Status:            StatusDelivered,
		ProviderMessageID: fmt.Sprintf("log-%d", time.Now().UnixNano()),
	}, nil
}

// Outbound is the outbound side of the SMS transport: it renders learning
// content into text and sends it through the configured gateway.
//
// It does not decide *what* to send. That is the scheduler's job. Keeping the
// two apart means the delivery policy can change without touching rendering,
// and rendering can be unit-tested without a gateway.
type Outbound struct {
	gateway   Gateway
	templates Templates
	logger    Logger
}

// NewOutbound builds an outbound sender.
func NewOutbound(gateway Gateway, templates Templates, logger Logger) *Outbound {
	return &Outbound{gateway: gateway, templates: templates, logger: Log(logger)}
}

// SendText delivers body to a number, splitting it into segments if needed.
//
// A body longer than the gateway's segment allowance is split here rather than
// sent oversized: providers reject over-long single messages, and a rejected
// assessment question is worse than two messages.
func (o *Outbound) SendText(ctx context.Context, to, body, reference string) error {
	if o.gateway == nil {
		return ErrNoGateway
	}

	chunks := Chunk(body)
	if len(chunks) == 0 {
		return nil
	}

	maxSegments := CapabilitiesOf(o.gateway).MaxSegments
	if maxSegments > 0 && len(chunks) > maxSegments {
		// Truncating silently would deliver a partial lesson and mark it
		// learned. Refusing loudly is the honest behaviour.
		return fmt.Errorf(
			"sms: message to %s needs %d segments but the gateway allows %d",
			to, len(chunks), maxSegments,
		)
	}

	for i, chunk := range chunks {
		// The segment index is part of the client reference so delivery
		// reports for a split message remain distinguishable.
		ref := reference
		if len(chunks) > 1 {
			ref = fmt.Sprintf("%s#%d/%d", reference, i+1, len(chunks))
		}

		receipt, err := o.gateway.Send(ctx, Message{
			To:              to,
			Body:            chunk,
			ClientReference: ref,
		})
		if err != nil {
			o.logger.Warn("sms send failed",
				"to", to, "reference", ref, "error", err)
			return fmt.Errorf("sms: send to %s: %w", to, err)
		}

		if receipt.Status.Terminal() && receipt.Status != StatusDelivered && receipt.Status != StatusSent {
			// Rejected: retrying the same number cannot help, and continuing
			// would keep texting somebody the provider has refused.
			o.logger.Warn("sms rejected",
				"to", to, "reference", ref, "status", receipt.Status, "error", receipt.Error)
			return &RejectedError{To: to, Status: receipt.Status, Reason: receipt.Error}
		}

		o.logger.Debug("sms sent", "to", to, "reference", ref, "status", receipt.Status)
	}

	return nil
}

// RejectedError indicates the provider refused a message outright.
type RejectedError struct {
	To     string
	Status DeliveryStatus
	Reason string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("sms: message to %s rejected (%s): %s", e.To, e.Status, e.Reason)
}

// Is lets errors.Is treat a rejection as terminal, matching the retry policy.
func (e *RejectedError) Is(target error) bool { return target == ErrGatewayUnavailable }

// SendQuestion renders and delivers a question.
func (o *Outbound) SendQuestion(ctx context.Context, to string, params QuestionParams, reference string) error {
	return o.SendText(ctx, to, o.templates.QuestionMessage(params), reference)
}

// SendLesson renders and delivers lesson content.
func (o *Outbound) SendLesson(ctx context.Context, to, title, body, reference string) error {
	return o.SendText(ctx, to, o.templates.LessonMessage(title, body), reference)
}

// SendControl delivers a control message such as a help reply or an opt-out
// confirmation.
func (o *Outbound) SendControl(ctx context.Context, to, body, reference string) error {
	return o.SendText(ctx, to, body, reference)
}

// StripToASCII removes characters a GSM-7 handset cannot render.
//
// Not applied to lesson bodies: silently altering educational content is worse
// than a slightly odd character. It exists for control messages, where an
// unexpected glyph is cosmetic.
func StripToASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 32 || r > 126 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
