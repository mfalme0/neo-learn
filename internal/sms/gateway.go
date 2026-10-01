// Package sms implements the SMS transport.
//
// It sits on the delivery side of the platform and knows nothing about how
// learning state is modelled. It renders content as text, delivers it through a
// pluggable Gateway, and turns an inbound reply back into a learner action by
// calling the same learning engine the web client calls.
//
// Nothing in this package writes learner state directly. An SMS answer is an
// event, identical in kind to one submitted from a browser.
package sms

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ConversationState mirrors the sms_conversation_state enum.
//
// Duplicated as a Go type rather than reusing the generated db enum so this
// package does not depend on the storage layer. Keeping them in step is a
// deliberate, small duplication: an inbound path that has to import sqlc
// output to ask "is a question pending" would blur the boundary this project is
// built around.
type ConversationState string

const (
	SmsStateIdle           ConversationState = "idle"
	SmsStateAwaitingAnswer ConversationState = "awaiting_answer"
	SmsStateAwaitingStart  ConversationState = "awaiting_start"
	SmsStateCompleted      ConversationState = "completed"
)

// AwaitingReply reports whether a learner could be mid-conversation.
func (s ConversationState) AwaitingReply() bool {
	return s == SmsStateAwaitingAnswer || s == SmsStateAwaitingStart
}

// SmsConversation is the transport state for one learner.
type SmsConversation struct {
	ID                  int64
	InstitutionID       int64
	LearnerID           int64
	State               ConversationState
	PendingAssessmentID *int64
	PendingQuestionID   *int64
	PendingLessonID     *int64
	PendingPosition     int
	PendingTotal        int
	OptedOut            bool
}

// OutboxMessage is one notification awaiting delivery, as the delivery planner
// sees it.
type OutboxMessage struct {
	OutboxID      int64
	InstitutionID int64
	LearnerID     int64
	EventID       string
	Topic         string
	To            string
}

// DeliveryStatus is the provider's verdict on a send attempt.
//
// The distinction between Rejected and Failed matters operationally: Rejected
// means the number is wrong or unsubscribed and retrying is pointless, while
// Failed means a transient problem where a retry might succeed.
type DeliveryStatus string

const (
	StatusQueued    DeliveryStatus = "queued"
	StatusSent      DeliveryStatus = "sent"
	StatusDelivered DeliveryStatus = "delivered"
	StatusRejected  DeliveryStatus = "rejected"
	StatusFailed    DeliveryStatus = "failed"
)

// Terminal reports whether no retry can change the outcome.
//
// A terminal result must not be retried: retrying a rejected number burns
// money on every attempt and, worse, keeps texting somebody who asked to stop.
func (s DeliveryStatus) Terminal() bool {
	switch s {
	case StatusDelivered, StatusSent, StatusRejected:
		return true
	default:
		return false
	}
}

// Message is an outbound SMS.
type Message struct {
	// To is an E.164 number, e.g. +254700000001.
	To string
	// Body is the full text. Gateway implementations are responsible for
	// splitting it into segments; see Chunk for the shared splitter.
	Body string
	// ClientReference ties this message back to the platform's own identifier,
	// so a delivery report can be matched to the outbox row that produced it.
	ClientReference string
}

// Receipt is the provider's response to a send attempt.
type Receipt struct {
	// Status is the provider's verdict.
	Status DeliveryStatus
	// ProviderMessageID is the provider's own identifier. Stored on the outbox
	// row so a delivery report arriving later can be matched to it.
	ProviderMessageID string
	// Error carries the reason when Status is rejected or failed. It is logged
	// but never shown to a learner.
	Error string
}

// ErrGatewayUnavailable indicates the provider could not be reached.
//
// Distinct from a rejection: an unavailable gateway is transient and the
// message stays queued.
var ErrGatewayUnavailable = errors.New("sms: gateway unavailable")

// Gateway is the seam between the platform and an SMS provider.
//
// This is the extension point the README's "add a channel without rewriting
// the engine" claim depends on. Adding WhatsApp or push means a second
// implementation of this interface, not a change to the learning engine.
type Gateway interface {
	// Send attempts delivery and returns the provider's verdict.
	Send(ctx context.Context, msg Message) (Receipt, error)

	// Name identifies the provider in logs and metrics.
	Name() string
}

// Capabilities describes what a gateway can do.
//
// A provider that cannot receive replies is a real constraint, not a
// hypothetical: a one-way SMS gateway can deliver a lesson but can never close
// the loop, which changes what the delivery scheduler is allowed to do.
type Capabilities struct {
	// Inbound means the provider can deliver received messages back to us.
	Inbound bool
	// MaxSegments is the segment count the provider will accept in one
	// request. Zero means unlimited.
	MaxSegments int
}

// DescribeCapabilities asks a gateway what it supports.
//
// Optional: gateways that do not implement it are assumed to support inbound
// with unlimited segments, which matches the common case.
type capabilitiesReporter interface {
	Capabilities() Capabilities
}

// CapabilitiesOf reports a gateway's capabilities, defaulting to permissive.
func CapabilitiesOf(g Gateway) Capabilities {
	if reporter, ok := g.(capabilitiesReporter); ok {
		return reporter.Capabilities()
	}
	return Capabilities{Inbound: true}
}

// ---------------------------------------------------------------------------
// Inbound
// ---------------------------------------------------------------------------

// InboundMessage is a message received from a learner.
type InboundMessage struct {
	// From is the sender's number in E.164 form.
	From string
	// Body is the text the learner sent, verbatim and untrimmed.
	Body string
	// ProviderMessageID is the provider's identifier for this inbound message.
	//
	// This is the basis of inbound idempotency. Gateways retry inbound
	// webhooks, so the same message can arrive two or three times; deriving the
	// event id from this value means a retry is a no-op rather than a second
	// recorded answer.
	ProviderMessageID string
	// ReceivedAt is the provider's timestamp. Preferred over the server clock,
	// because it precedes our own processing by however long the webhook took.
	ReceivedAt time.Time
}

// Commands a learner can send that are not answers.
const (
	CommandStop  = "STOP"
	CommandStart = "START"
	CommandHelp  = "HELP"
	CommandNext  = "NEXT"
)

// CommandDone acknowledges a lesson.
//
// A distinct command rather than an alias of NEXT: the lesson message tells the
// learner to reply DONE, and a parser that does not recognise the word the
// rendered message actually uses is a mismatch between what we say and what we
// understand.
const CommandDone = "DONE"

// NormalizedCommand classifies an inbound body that is a control word rather
// than an answer.
//
// Matching is deliberately forgiving: a learner on a feature phone types
// "stop", "Stop", or " STOP. ", and requiring an exact match would mean a
// learner who cannot get the casing right keeps receiving messages after asking
// to stop, which is the kind of bug that damages trust permanently.
func NormalizedCommand(body string) string {
	trimmed := strings.TrimSpace(body)

	// A bare question mark is help. Checked before the punctuation trim, which
	// would otherwise strip it away.
	if strings.EqualFold(trimmed, "?") {
		return CommandHelp
	}

	// Some handsets append punctuation or a trailing space to quick-reply
	// words, so both are stripped before matching.
	trimmed = strings.TrimSpace(strings.Trim(trimmed, ".!?,;: \t"))
	trimmed = strings.ToUpper(trimmed)

	switch trimmed {
	case CommandStop, "UNSUBSCRIBE", "CANCEL", "END", "QUIT":
		return CommandStop
	case CommandStart, "SUBSCRIBE", "JOIN", "RESUME":
		return CommandStart
	case CommandHelp:
		return CommandHelp
	case CommandDone:
		return CommandDone
	case CommandNext, "CONTINUE", "MORE":
		return CommandNext
	default:
		return ""
	}
}

// ValidateE164 reports whether a number is plausibly routable.
//
// This is not full E.164 validation -- country-length rules are not worth
// reimplementing -- but it does catch the cases that actually occur: a number
// stored without a plus, a number with spaces, an empty string from a form that
// was left blank.
func ValidateE164(number string) bool {
	trimmed := strings.TrimSpace(number)
	if len(trimmed) < 8 || len(trimmed) > 16 {
		return false
	}
	if !strings.HasPrefix(trimmed, "+") {
		return false
	}
	for _, r := range trimmed[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// NormalizeE164 converts a stored or dialled number to canonical form.
//
// Strips the punctuation a learner might type or an operator might include in a
// delivery report. Returns an error rather than a best-effort string, because a
// silently wrong number sends a learner's answers to a stranger.
func NormalizeE164(number string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '-', '(', ')', '.':
			return -1
		default:
			return r
		}
	}, strings.TrimSpace(number))

	if !ValidateE164(cleaned) {
		return "", fmt.Errorf("sms: %q is not a valid E.164 number", number)
	}
	return cleaned, nil
}
