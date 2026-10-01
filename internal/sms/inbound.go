package sms

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/neolearn/neolearn/internal/learning"
)

// jsonMarshal is a seam so the payload encoder can be swapped in tests.
var jsonMarshal = json.Marshal

// inboundNamespace seeds the UUID v5 derivation for inbound event ids.
//
// A fixed namespace constant, not a random one: the derivation has to be stable
// across processes and restarts, or a gateway retry would mint a different id
// for the same message and be recorded as a second answer.
var inboundNamespace = uuid.MustParse("6f9d1c2e-4a7b-5c3d-8e1f-2b3a4c5d6e7f")

// InboundEventID derives the learning event id for an inbound SMS.
//
// This is where inbound idempotency comes from. Gateways retry webhook
// deliveries, so the same reply can arrive more than once; deriving the id from
// the provider's own message id means the second arrival collides with the
// UNIQUE constraint and is discarded by the learning engine.
//
// Deriving rather than generating is the whole point. A generated UUID would be
// unique on every arrival, so every retry would be scored as a fresh answer.
//
// The institution id is mixed in so the same provider message id reported
// against two institutions cannot collide.
func InboundEventID(providerMessageID string, institutionID int64) uuid.UUID {
	digest := sha256.Sum256([]byte(strings.TrimSpace(providerMessageID)))

	name := hex.EncodeToString(digest[:]) + ":" + itoa(institutionID)
	return uuid.NewSHA1(inboundNamespace, []byte(name))
}

func itoa(v int64) string {
	// strconv without importing it for one call site would be an odd trade the
	// other way; keeping it explicit avoids the allocation dance.
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ParsedReply is the outcome of interpreting an inbound message body.
type ParsedReply struct {
	// Kind is what the learner was doing.
	Kind ReplyKind

	// Command is set when Kind is ReplyCommand.
	Command string

	// ChoiceLabel is the uppercase single-letter answer, when Kind is
	// ReplyAnswer.
	ChoiceLabel string

	// Raw is the original body, retained for the "did not understand" echo.
	Raw string
}

// ReplyKind classifies an inbound reply.
type ReplyKind string

const (
	// ReplyAnswer is a choice for the pending question.
	ReplyAnswer ReplyKind = "answer"
	// ReplyCommand is a control word.
	ReplyCommand ReplyKind = "command"
	// ReplyUnrecognized could not be interpreted.
	ReplyUnrecognized ReplyKind = "unrecognized"
)

// ParseReply interprets an inbound message body.
//
// Order matters: a control word is checked before an answer, because a learner
// replying "NEXT" should not have it read as a choice if an assessment happens
// to offer a choice labelled N.
//
// A single letter is only accepted as an answer when it is in `valid` --
// otherwise a learner who replies "A" to a lesson prompt with no pending
// question would get an error about a question that does not exist.
func ParseReply(body string, valid []string) ParsedReply {
	raw := body
	trimmed := strings.TrimSpace(body)

	if trimmed == "" {
		return ParsedReply{Kind: ReplyUnrecognized, Raw: raw}
	}

	if command := NormalizedCommand(trimmed); command != "" {
		return ParsedReply{Kind: ReplyCommand, Command: command, Raw: raw}
	}

	// A bare label, possibly with surrounding noise: "b", " B.", "answer b".
	// A learner typing a bare letter is the single most common case, so it is
	// matched loosely; anything longer is only accepted if it is exactly a
	// label, to avoid swallowing "None of the above".
	candidate := strings.Trim(strings.TrimSpace(trimmed), ".!?,;: ")
	candidate = strings.ToUpper(candidate)

	if candidate == "" {
		return ParsedReply{Kind: ReplyUnrecognized, Raw: raw}
	}

	if len([]rune(candidate)) == 1 {
		for _, label := range valid {
			if strings.EqualFold(label, candidate) {
				return ParsedReply{
					Kind:        ReplyAnswer,
					ChoiceLabel: strings.ToUpper(label),
					Raw:         raw,
				}
			}
		}
	}

	return ParsedReply{Kind: ReplyUnrecognized, Raw: raw}
}

// BuildAnswerEvent assembles the learning event for an answered question.
//
// Note there is no learner id parameter. The learner is taken from the resolved
// conversation, never from the message body: a forged body must not be able to
// choose whose state it changes.
func BuildAnswerEvent(params AnswerEventParams) learning.Event {
	return learning.Event{
		EventID:       params.EventID,
		InstitutionID: params.InstitutionID,
		LearnerID:     params.LearnerID,
		QuestionID:    &params.QuestionID,
		AssessmentID:  &params.AssessmentID,
		Kind:          learning.KindQuestionAnswered,
		Source:        learning.SourceSMS,
		Payload:       mustPayload(learning.AnswerPayload{ChoiceLabel: params.ChoiceLabel}),
		// The gateway's timestamp, not the server's clock. It is earlier than
		// our processing time by however long the webhook took, and that gap is
		// exactly the delivery latency worth measuring.
		OccurredAt: params.OccurredAt,
	}
}

// BuildLessonEvent assembles the learning event for a completed lesson.
func BuildLessonEvent(params LessonEventParams) learning.Event {
	return learning.Event{
		EventID:       params.EventID,
		InstitutionID: params.InstitutionID,
		LearnerID:     params.LearnerID,
		LessonID:      &params.LessonID,
		Kind:          learning.KindLessonCompleted,
		Source:        learning.SourceSMS,
		Payload:       mustPayload(nil),
		OccurredAt:    params.OccurredAt,
	}
}

// AnswerEventParams are the inputs for an answer event.
type AnswerEventParams struct {
	EventID       uuid.UUID
	InstitutionID int64
	LearnerID     int64
	QuestionID    int64
	AssessmentID  int64
	ChoiceLabel   string
	OccurredAt    time.Time
}

// LessonEventParams are the inputs for a lesson event.
type LessonEventParams struct {
	EventID       uuid.UUID
	InstitutionID int64
	LearnerID     int64
	LessonID      int64
	OccurredAt    time.Time
}

func mustPayload(v any) []byte {
	if v == nil {
		return []byte("{}")
	}
	b, err := jsonMarshal(v)
	if err != nil {
		// Only reachable if a struct here gains an unmarshalable field, which
		// is a review-time concern rather than a runtime one.
		panic("sms: encode payload: " + err.Error())
	}
	return b
}
