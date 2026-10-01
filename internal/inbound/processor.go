// Package inbound processes replies received from learners by SMS.
//
// This is the transport half of the read path: a message arrives, is resolved
// to a learner and a pending question, and becomes a learning event through the
// same engine the web client calls.
//
// It is a separate package from sms because it has different failure
// characteristics. Sending is our responsibility and retried by the outbox
// worker; receiving is the gateway's responsibility and arrives at a public HTTP
// endpoint, so it is authenticated by a shared secret rather than by a session.
package inbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/learning"
	"github.com/neolearn/neolearn/internal/sms"
)

// Result describes what happened to an inbound message.
type Result struct {
	// Handled is true when the message was understood and acted on.
	Handled bool
	// Reply is the text to send back, if any.
	Reply string
	// Duplicate is true when this exact provider message had already been
	// processed.
	Duplicate bool
	// Outcome labels the handling for logging and metrics.
	Outcome Outcome
}

// Outcome enumerates what the inbound path did.
type Outcome string

const (
	OutcomeAnswered      Outcome = "answered"
	OutcomeDuplicate     Outcome = "duplicate"
	OutcomeCommand       Outcome = "command"
	OutcomeUnrecognized  Outcome = "unrecognized"
	OutcomeOptedOut      Outcome = "opted_out"
	OutcomeUnknownSender Outcome = "unknown_sender"
	OutcomeNoPending     Outcome = "no_pending_question"
	OutcomeRejected      Outcome = "rejected"
)

// Errors returned for conditions the caller should log rather than retry.
//
// The gateway will retry a non-2xx webhook, so a permanent failure must still
// answer 2xx or the provider will keep redelivering a message that can never
// succeed.
var (
	ErrUnknownSender = errors.New("inbound: sender is not a known learner")
	ErrNoTenant      = errors.New("inbound: no institution configured for this sender id")
)

// Processor handles inbound messages.
type Processor struct {
	queries   *db.Queries
	learning  *learning.Service
	delivery  *sms.Delivery
	templates sms.Templates
	logger    *slog.Logger
}

// NewProcessor builds an inbound processor.
func NewProcessor(
	queries *db.Queries,
	learningService *learning.Service,
	delivery *sms.Delivery,
	templates sms.Templates,
	logger *slog.Logger,
) *Processor {
	// A nil logger falls back to the default rather than panicking on the
	// first delivery: inbound handling must not be able to crash the process.
	if logger == nil {
		logger = slog.Default()
	}

	return &Processor{
		queries:   queries,
		learning:  learningService,
		delivery:  delivery,
		templates: templates,
		logger:    logger,
	}
}

// Handle processes one inbound message.
//
// The learner and institution are resolved from the sender number alone. Nothing
// in the message body can influence whose state changes: a forged "from" is the
// only thing that decides who is affected.
func (p *Processor) Handle(ctx context.Context, msg sms.InboundMessage) (Result, error) {
	from, err := sms.NormalizeE164(msg.From)
	if err != nil {
		p.logger.Warn("inbound message has an unusable sender",
			"from", msg.From, "error", err)
		// Not retryable: the same message will always have the same bad
		// sender. Acknowledging is the honest answer.
		return Result{Handled: false, Outcome: OutcomeUnknownSender}, nil
	}

	institutionID, err := p.resolveInstitution(ctx, from)
	if err != nil {
		if errors.Is(err, ErrUnknownSender) {
			p.logger.Info("inbound from an unknown number", "from", from)
			return Result{Handled: false, Outcome: OutcomeUnknownSender}, nil
		}
		return Result{}, err
	}

	learner, err := p.queries.GetUserByMSISDN(ctx, db.GetUserByMSISDNParams{
		Msisdn:        &from,
		InstitutionID: institutionID,
	})
	if err != nil {
		return Result{}, fmt.Errorf("inbound: resolve learner: %w", err)
	}

	occurredAt := msg.ReceivedAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}

	// Deduplicate before anything else.
	//
	// The event id is derived from the provider's message id, so a match means
	// this exact delivery has already been processed. Doing this first matters:
	// by the time a gateway retry is routed, the conversation may have advanced
	// or closed, and routing against stale state produces a confusing "not
	// understood" reply rather than a silent success.
	//
	// No reply is sent for a duplicate. The learner already got one.
	if msg.ProviderMessageID != "" {
		eventID := sms.InboundEventID(msg.ProviderMessageID, institutionID)

		seen, err := p.queries.LearningEventExists(ctx, uuidParam(eventID))
		if err != nil {
			return Result{}, fmt.Errorf("inbound: check for duplicate: %w", err)
		}
		if seen {
			p.logger.Debug("inbound duplicate ignored",
				"learner_id", learner.ID, "provider_message_id", msg.ProviderMessageID)
			return Result{Handled: true, Duplicate: true, Outcome: OutcomeDuplicate}, nil
		}
	}

	return p.handleForLearner(ctx, institutionID, learner.ID, msg, occurredAt)
}

// uuidParam renders a UUID for the query layer.
func uuidParam(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// resolveInstitution maps a sender number to an institution.
//
// The sender id is configured per institution on the provider side, so an
// inbound webhook's sender id is authoritative about which tenant it belongs to.
// This is checked before the learner lookup so a number that exists at two
// schools is routed to the right one rather than to whichever row sorts first.
func (p *Processor) resolveInstitution(ctx context.Context, from string) (int64, error) {
	institutions, err := p.institutionsWithNumber(ctx, from)
	if err != nil {
		return 0, err
	}

	switch len(institutions) {
	case 0:
		return 0, ErrUnknownSender
	case 1:
		return institutions[0], nil
	default:
		// Ambiguous: the same number is a learner at more than one
		// institution. Guessing would let a message from one school be applied
		// to another's records.
		return 0, fmt.Errorf("%w: %s is registered at %d institutions; configure a distinct sender id per institution",
			ErrNoTenant, from, len(institutions))
	}
}

func (p *Processor) institutionsWithNumber(ctx context.Context, from string) ([]int64, error) {
	rows, err := p.queries.GetInstitutionsForMSISDN(ctx, &from)
	if err != nil {
		return nil, fmt.Errorf("inbound: resolve institution: %w", err)
	}
	return rows, nil
}

func (p *Processor) handleForLearner(
	ctx context.Context,
	institutionID, learnerID int64,
	msg sms.InboundMessage,
	occurredAt time.Time,
) (Result, error) {
	conversation, err := p.delivery.LoadPendingQuestion(ctx, institutionID, learnerID)
	if err != nil {
		return Result{}, fmt.Errorf("inbound: load conversation: %w", err)
	}

	// A control word is handled before anything else, so a STOP takes effect
	// even when the learner has a question pending.
	command := sms.NormalizedCommand(msg.Body)

	// DONE is routed to the lesson path rather than the generic command
	// handler: it is only meaningful when a lesson is pending, and a DONE sent
	// against a question is not a command at all.
	if command == sms.CommandDone {
		if conversation != nil && conversation.State == sms.SmsStateAwaitingStart {
			return p.handleLessonReply(ctx, institutionID, learnerID, msg, occurredAt, conversation)
		}
		return Result{
			Handled: true,
			Outcome: OutcomeUnrecognized,
			Reply:   p.templates.NotUnderstoodMessage(msg.Body, nil),
		}, nil
	}

	if command != "" {
		return p.handleCommand(ctx, institutionID, learnerID, command, conversation)
	}

	if conversation == nil || !conversation.State.AwaitingReply() {
		// Nothing to answer. Asking what they meant via HELP is better than
		// silence, and far better than an error message.
		return Result{
			Handled: true,
			Outcome: OutcomeNoPending,
			Reply:   p.templates.HelpMessage(false),
		}, nil
	}

	if conversation.PendingQuestionID == nil {
		// Awaiting a lesson rather than an answer.
		return p.handleLessonReply(ctx, institutionID, learnerID, msg, occurredAt, conversation)
	}

	validLabels, err := p.validLabels(ctx, *conversation.PendingQuestionID, institutionID)
	if err != nil {
		return Result{}, err
	}

	reply := sms.ParseReply(msg.Body, validLabels)
	if reply.Kind != sms.ReplyAnswer {
		return Result{
			Handled: true,
			Outcome: OutcomeUnrecognized,
			Reply:   p.templates.NotUnderstoodMessage(msg.Body, validLabels),
		}, nil
	}

	// The event id is derived from the provider's message id, so a gateway
	// retry of this webhook is discarded by the learning engine rather than
	// scored as a second answer.
	eventID := sms.InboundEventID(msg.ProviderMessageID, institutionID)

	event := sms.BuildAnswerEvent(sms.AnswerEventParams{
		EventID:       eventID,
		InstitutionID: institutionID,
		LearnerID:     learnerID,
		QuestionID:    *conversation.PendingQuestionID,
		AssessmentID:  *conversation.PendingAssessmentID,
		ChoiceLabel:   reply.ChoiceLabel,
		OccurredAt:    occurredAt,
	})

	ingested, err := p.learning.Ingest(ctx, event)
	if err != nil {
		if errors.Is(err, learning.ErrChoiceNotFound) {
			// The learner replied with a label the question does not offer.
			return Result{
				Handled: true,
				Outcome: OutcomeRejected,
				Reply:   p.templates.NotUnderstoodMessage(msg.Body, validLabels),
			}, nil
		}
		return Result{}, fmt.Errorf("inbound: ingest answer: %w", err)
	}

	if ingested.Duplicate {
		// The gateway retried. Re-acknowledging would cost a message for
		// nothing, so this returns quietly.
		p.logger.Debug("inbound duplicate ignored",
			"learner_id", learnerID, "event_id", eventID)
		return Result{Handled: true, Duplicate: true, Outcome: OutcomeDuplicate}, nil
	}

	return p.acknowledgeAnswer(ctx, institutionID, learnerID, conversation, ingested.Graded)
}

// acknowledgeAnswer builds the reply after a graded answer.
//
// Exactly one message goes back. A learner who receives a verdict and a
// follow-up question as two separate texts pays twice and has to work out which
// was which, so the advance message is preferred and carries the outcome.
func (p *Processor) acknowledgeAnswer(
	ctx context.Context,
	institutionID, learnerID int64,
	conversation *sms.SmsConversation,
	graded *learning.GradedAnswer,
) (Result, error) {
	// Advance first: if the assessment is finished, its completion message is
	// the whole reply and there is nothing else to say.
	next, err := p.delivery.AdvanceAssessment(ctx, institutionID, learnerID, *conversation.PendingAssessmentID)
	if err != nil {
		return Result{}, fmt.Errorf("inbound: advance assessment: %w", err)
	}

	if next != nil && isTerminalAdvance(next.Body) {
		// The assessment is complete; its message already carries the score.
		return Result{Handled: true, Outcome: OutcomeAnswered, Reply: next.Body}, nil
	}

	if next != nil {
		// Prefix the verdict onto the next question so it is one message.
		return Result{
			Handled: true,
			Outcome: OutcomeAnswered,
			Reply:   p.verdict(graded, institutionID) + "\n\n" + next.Body,
		}, nil
	}

	// No next question and no completion message: the conversation has ended.
	return Result{
		Handled: true,
		Outcome: OutcomeAnswered,
		Reply:   p.verdict(graded, institutionID),
	}, nil
}

// isTerminalAdvance reports whether an advance message ends the assessment.
func isTerminalAdvance(body string) bool {
	return body != "" && strings.Contains(body, "Assessment complete")
}

// verdict renders the immediate feedback for a graded answer.
func (p *Processor) verdict(graded *learning.GradedAnswer, institutionID int64) string {
	ctx := context.Background()

	if graded == nil {
		// Should not happen for an accepted answer, but replying with nothing
		// would leave the learner with no idea whether they were heard.
		return p.templates.HelpMessage(true)
	}

	line := fmt.Sprintf("Score: %d/%d", graded.MarksAwarded, graded.MarksTotal)

	if graded.Correct {
		return p.templates.CorrectAnswerMessage(sms.ResultParams{ScoreLine: line})
	}

	// Naming the right answer is the teaching part. A bare "wrong" teaches
	// nothing, and this is the only moment the learner is told.
	label, body := p.correctChoice(ctx, graded.QuestionID, institutionID)
	return p.templates.IncorrectAnswerMessage(sms.ResultParams{
		ScoreLine:     line,
		CorrectChoice: label,
		CorrectBody:   body,
	})
}

func (p *Processor) handleLessonReply(
	ctx context.Context,
	institutionID, learnerID int64,
	msg sms.InboundMessage,
	occurredAt time.Time,
	conversation *sms.SmsConversation,
) (Result, error) {
	// DONE is the word the lesson message actually tells the learner to use, so
	// it has to be the one accepted here.
	if normalized := sms.NormalizedCommand(msg.Body); normalized != sms.CommandDone {
		return Result{
			Handled: true,
			Outcome: OutcomeUnrecognized,
			Reply:   p.templates.NotUnderstoodMessage(msg.Body, []string{sms.CommandDone}),
		}, nil
	}

	eventID := sms.InboundEventID(msg.ProviderMessageID, institutionID)
	event := sms.BuildLessonEvent(sms.LessonEventParams{
		EventID:       eventID,
		InstitutionID: institutionID,
		LearnerID:     learnerID,
		LessonID:      *conversation.PendingLessonID,
		OccurredAt:    occurredAt,
	})

	ingested, err := p.learning.Ingest(ctx, event)
	if err != nil {
		return Result{}, fmt.Errorf("inbound: ingest lesson completion: %w", err)
	}
	if ingested.Duplicate {
		return Result{Handled: true, Duplicate: true, Outcome: OutcomeDuplicate}, nil
	}

	lessonTitle, courseID := p.lessonContext(ctx, *conversation.PendingLessonID, institutionID)

	next, err := p.delivery.AdvanceLesson(ctx, institutionID, learnerID, courseID)
	if err != nil {
		return Result{}, fmt.Errorf("inbound: advance lesson: %w", err)
	}

	if next != nil {
		return Result{Handled: true, Outcome: OutcomeAnswered, Reply: next.Body}, nil
	}

	return Result{
		Handled: true,
		Outcome: OutcomeAnswered,
		Reply:   p.templates.LessonCompleteMessage(lessonTitle),
	}, nil
}

func (p *Processor) handleCommand(
	ctx context.Context,
	institutionID, learnerID int64,
	command string,
	conversation *sms.SmsConversation,
) (Result, error) {
	switch command {
	case sms.CommandStop:
		// Honouring STOP must not depend on a conversation existing.
		if err := p.queries.SetSmsOptOut(ctx, db.SetSmsOptOutParams{
			InstitutionID: institutionID,
			LearnerID:     learnerID,
			OptedOut:      true,
		}); err != nil {
			return Result{}, fmt.Errorf("inbound: record opt-out: %w", err)
		}
		p.logger.Info("learner opted out of SMS", "learner_id", learnerID, "institution_id", institutionID)
		return Result{Handled: true, Outcome: OutcomeOptedOut, Reply: p.templates.StopMessage()}, nil

	case sms.CommandStart:
		if err := p.queries.SetSmsOptOut(ctx, db.SetSmsOptOutParams{
			InstitutionID: institutionID,
			LearnerID:     learnerID,
			OptedOut:      false,
		}); err != nil {
			return Result{}, fmt.Errorf("inbound: record opt-in: %w", err)
		}
		return Result{Handled: true, Outcome: OutcomeCommand, Reply: p.templates.StartMessage()}, nil

	case sms.CommandHelp:
		hasPending := conversation != nil && conversation.State.AwaitingReply()
		return Result{Handled: true, Outcome: OutcomeCommand, Reply: p.templates.HelpMessage(hasPending)}, nil

	default:
		return Result{
			Handled: true,
			Outcome: OutcomeUnrecognized,
			Reply:   p.templates.HelpMessage(conversation != nil && conversation.State.AwaitingReply()),
		}, nil
	}
}

func (p *Processor) validLabels(ctx context.Context, questionID, institutionID int64) ([]string, error) {
	choices, err := p.queries.ListChoicesForSms(ctx, db.ListChoicesForSmsParams{
		QuestionID:    questionID,
		InstitutionID: institutionID,
	})
	if err != nil {
		return nil, fmt.Errorf("inbound: load choices: %w", err)
	}

	labels := make([]string, 0, len(choices))
	for _, choice := range choices {
		labels = append(labels, choice.Label)
	}
	return labels, nil
}

// correctChoice looks up the right answer so an incorrect response can explain
// itself.
//
// The label and body are read only here, immediately after grading, never as
// part of a question prompt: the answer key must not be visible in any message
// the learner sees before answering.
func (p *Processor) correctChoice(ctx context.Context, questionID, institutionID int64) (string, string) {
	label, err := p.queries.GetCorrectChoiceLabel(ctx, db.GetCorrectChoiceLabelParams{
		QuestionID:    questionID,
		InstitutionID: institutionID,
	})
	if err != nil || label == "" {
		return "", ""
	}

	choice, err := p.queries.GetChoiceForQuestion(ctx, db.GetChoiceForQuestionParams{
		QuestionID:    questionID,
		InstitutionID: institutionID,
		Label:         label,
	})
	if err != nil {
		return "", ""
	}
	return choice.Label, choice.Body
}

func (p *Processor) lessonContext(ctx context.Context, lessonID, institutionID int64) (string, int64) {
	lesson, err := p.queries.GetLessonForSms(ctx, db.GetLessonForSmsParams{
		ID:            lessonID,
		InstitutionID: institutionID,
	})
	if err != nil {
		return "", 0
	}
	return lesson.Title, lesson.CourseID
}
