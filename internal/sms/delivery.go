package sms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/learning"
)

// dbState converts a ConversationState to the generated enum.
//
// An explicit conversion rather than a type alias: the generated type is an
// internal implementation detail of the storage layer, and letting it leak into
// this package's signatures would tie SMS delivery to sqlc's naming.
func dbState(state ConversationState) db.SmsConversationState {
	return db.SmsConversationState(state)
}

// Delivery decides what, if anything, to send a learner in response to an event.
//
// It is separate from rendering (which turns content into text) and from the
// outbox worker (which handles retries) because "what does this learner need
// next" is the product decision in the SMS path, and it is the part most likely
// to change.
type Delivery struct {
	queries   *db.Queries
	templates Templates
	logger    Logger
}

// NewDelivery builds a delivery planner.
func NewDelivery(queries *db.Queries, templates Templates, logger Logger) *Delivery {
	return &Delivery{queries: queries, templates: templates, logger: Log(logger)}
}

// Notification is a message the planner wants sent.
type Notification struct {
	To   string
	Body string
	// Reference ties the message to the event that caused it, so a delivery
	// report can be attributed.
	Reference string
}

// Plan renders the response to an outbox notification.
//
// The payload identifies which learner and which event; what to send is
// derived from the learner's current state rather than the event alone. That
// matters because by the time a notification is delivered the learner may have
// already answered in the app, and a "Question 3 of 10" text would then be
// wrong.
//
// Returning nil means send nothing, which is a normal outcome rather than an
// error: a learner who is opted out, or already finished, should not be texted.
func (d *Delivery) Plan(ctx context.Context, msg OutboxMessage) (*Notification, error) {
	conversation, err := d.loadConversation(ctx, msg)
	if err != nil || conversation == nil {
		return nil, err
	}

	if conversation.OptedOut {
		// A learner who texted STOP must not be messaged, whatever the queue
		// says. Checking here rather than only at send time means the opt-out
		// takes effect on the very next message.
		d.logger.Debug("sms notification suppressed by opt-out",
			"learner_id", msg.LearnerID)
		return nil, nil
	}

	switch conversation.State {
	case SmsStateAwaitingAnswer:
		return d.planNextQuestion(ctx, msg, *conversation)
	case SmsStateAwaitingStart:
		return d.planLesson(ctx, msg, *conversation)
	default:
		return nil, nil
	}
}

// loadConversation reads the conversation, translating "no row" into a nil
// conversation with no error: a learner who has never been messaged simply has
// nothing pending, which is not a failure.
func (d *Delivery) loadConversation(ctx context.Context, msg OutboxMessage) (*SmsConversation, error) {
	row, err := d.queries.GetSmsConversation(ctx, db.GetSmsConversationParams{
		InstitutionID: msg.InstitutionID,
		LearnerID:     msg.LearnerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("sms: load conversation: %w", err)
	}

	return &SmsConversation{
		ID:                  row.ID,
		InstitutionID:       row.InstitutionID,
		LearnerID:           row.LearnerID,
		State:               ConversationState(row.State),
		PendingAssessmentID: row.PendingAssessmentID,
		PendingQuestionID:   row.PendingQuestionID,
		PendingLessonID:     row.PendingLessonID,
		PendingPosition:     int(row.PendingPosition),
		PendingTotal:        int(row.PendingTotal),
		OptedOut:            row.OptedOut,
	}, nil
}

func (d *Delivery) planNextQuestion(
	ctx context.Context,
	msg OutboxMessage,
	conversation SmsConversation,
) (*Notification, error) {
	assessmentID := *conversation.PendingAssessmentID
	total, err := d.assessmentTotal(ctx, msg.InstitutionID, assessmentID)
	if err != nil {
		return nil, err
	}

	// Start from -1 so the first unanswered question at position 0 is found:
	// the field stores the last position asked, not the next to ask.
	next, err := d.queries.GetNextUnansweredQuestion(ctx, db.GetNextUnansweredQuestionParams{
		AssessmentID:  assessmentID,
		InstitutionID: msg.InstitutionID,
		Position:      int32(conversation.PendingPosition - 1),
		LearnerID:     msg.LearnerID,
		Limit:         1,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Every question is answered correctly: the assessment is done.
			return d.planAssessmentComplete(ctx, msg, conversation, total)
		}
		return nil, fmt.Errorf("sms: next question: %w", err)
	}

	body, err := d.renderQuestion(ctx, msg.InstitutionID, assessmentID, next, total)
	if err != nil {
		return nil, err
	}

	// Record what is now pending before the send is attempted, so a second
	// notification arriving while this one is in flight does not ask the same
	// question twice.
	if err := d.setPendingQuestion(ctx, msg.InstitutionID, msg.LearnerID, assessmentID, next.ID, next.Position, total, conversation.OptedOut); err != nil {
		return nil, err
	}

	body.Reference = msg.EventID
	body.To = msg.To
	return body, nil
}

// renderQuestion builds the message for one question and records the
// conversation state.
func (d *Delivery) renderQuestion(
	ctx context.Context,
	institutionID, assessmentID int64,
	next db.GetNextUnansweredQuestionRow,
	total int,
) (*Notification, error) {
	question, err := d.queries.GetQuestionForSms(ctx, db.GetQuestionForSmsParams{
		ID:            next.ID,
		InstitutionID: institutionID,
	})
	if err != nil {
		return nil, fmt.Errorf("sms: load question: %w", err)
	}

	choices, err := d.queries.ListChoicesForSms(ctx, db.ListChoicesForSmsParams{
		QuestionID:    next.ID,
		InstitutionID: institutionID,
	})
	if err != nil {
		return nil, fmt.Errorf("sms: load choices: %w", err)
	}
	if len(choices) == 0 {
		// A question with no choices cannot be answered by SMS at all. Sending
		// it would produce a prompt the learner cannot reply to.
		return nil, fmt.Errorf("sms: question %d has no choices and cannot be delivered by SMS", next.ID)
	}

	return &Notification{
		Body: d.templates.QuestionMessage(QuestionParams{
			AssessmentTitle: question.AssessmentTitle,
			Prompt:          question.Prompt,
			// Positions are 0-based in the database and 1-based to a learner.
			Position: int(next.Position) + 1,
			Total:    total,
			Choices:  toChoices(choices),
		}),
	}, nil
}

func (d *Delivery) setPendingQuestion(
	ctx context.Context,
	institutionID, learnerID, assessmentID, questionID int64,
	position int32,
	total int,
	optedOut bool,
) error {
	if _, err := d.queries.UpsertSmsConversation(ctx, db.UpsertSmsConversationParams{
		InstitutionID:       institutionID,
		LearnerID:           learnerID,
		State:               dbState(SmsStateAwaitingAnswer),
		PendingAssessmentID: &assessmentID,
		PendingQuestionID:   &questionID,
		PendingLessonID:     nil,
		PendingPosition:     position,
		PendingTotal:        int32(total),
		OptedOut:            optedOut,
	}); err != nil {
		return fmt.Errorf("sms: update conversation: %w", err)
	}
	return nil
}

func (d *Delivery) planLesson(
	ctx context.Context,
	msg OutboxMessage,
	conversation SmsConversation,
) (*Notification, error) {
	lesson, err := d.queries.GetLessonForSms(ctx, db.GetLessonForSmsParams{
		ID:            *conversation.PendingLessonID,
		InstitutionID: msg.InstitutionID,
	})
	if err != nil {
		return nil, fmt.Errorf("sms: load lesson: %w", err)
	}

	return &Notification{
		To:        msg.To,
		Body:      d.templates.LessonMessage(lesson.Title, lesson.BodyMarkdown),
		Reference: msg.EventID,
	}, nil
}

func (d *Delivery) planAssessmentComplete(
	ctx context.Context,
	msg OutboxMessage,
	conversation SmsConversation,
	total int,
) (*Notification, error) {
	counts, err := d.queries.CountCorrectAnswers(ctx, db.CountCorrectAnswersParams{
		AssessmentID: *conversation.PendingAssessmentID,
		LearnerID:    msg.LearnerID,
	})
	if err != nil {
		return nil, fmt.Errorf("sms: count answers: %w", err)
	}

	// Nothing is pending any more, so the conversation is closed. Leaving it
	// awaiting an answer would invite a reply that can no longer be graded.
	if err := d.closeConversation(ctx, msg, SmsStateCompleted); err != nil {
		return nil, err
	}

	correct := int(counts.Correct)
	if total > 0 && correct > total {
		correct = total
	}

	return &Notification{
		To: msg.To,
		Body: d.templates.CorrectAnswerMessage(ResultParams{
			ScoreLine: fmt.Sprintf("Score: %d/%d", correct, total),
			Completed: true,
		}),
		Reference: msg.EventID,
	}, nil
}

// ---------------------------------------------------------------------------
// Advancing after an inbound reply
// ---------------------------------------------------------------------------

// AdvanceAssessment picks the next unanswered question after an answer.
//
// This is what keeps a SMS learner moving: without it the learner has answered
// and then receives nothing, which is indistinguishable from a dropped message.
func (d *Delivery) AdvanceAssessment(
	ctx context.Context,
	institutionID, learnerID, assessmentID int64,
) (*Notification, error) {
	total, err := d.assessmentTotal(ctx, institutionID, assessmentID)
	if err != nil {
		return nil, err
	}

	next, err := d.queries.GetNextUnansweredQuestion(ctx, db.GetNextUnansweredQuestionParams{
		AssessmentID:  assessmentID,
		InstitutionID: institutionID,
		Position:      -1,
		LearnerID:     learnerID,
		Limit:         1,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return d.finishAssessment(ctx, institutionID, learnerID, assessmentID, total)
		}
		return nil, fmt.Errorf("sms: next question: %w", err)
	}

	notification, err := d.renderQuestion(ctx, institutionID, assessmentID, next, total)
	if err != nil {
		return nil, err
	}

	if err := d.setPendingQuestion(ctx, institutionID, learnerID, assessmentID, next.ID, next.Position, total, false); err != nil {
		return nil, err
	}

	return notification, nil
}

func (d *Delivery) finishAssessment(
	ctx context.Context,
	institutionID, learnerID, assessmentID int64,
	total int,
) (*Notification, error) {
	counts, err := d.queries.CountCorrectAnswers(ctx, db.CountCorrectAnswersParams{
		AssessmentID: assessmentID,
		LearnerID:    learnerID,
	})
	if err != nil {
		return nil, fmt.Errorf("sms: count answers: %w", err)
	}

	if err := d.queries.CloseSmsConversation(ctx, db.CloseSmsConversationParams{
		InstitutionID: institutionID,
		LearnerID:     learnerID,
		State:         dbState(SmsStateCompleted),
	}); err != nil {
		return nil, fmt.Errorf("sms: close conversation: %w", err)
	}

	correct := int(counts.Correct)
	if total > 0 && correct > total {
		correct = total
	}

	return &Notification{
		Body: d.templates.CorrectAnswerMessage(ResultParams{
			ScoreLine: fmt.Sprintf("Score: %d/%d", correct, total),
			Completed: true,
		}),
	}, nil
}

// AdvanceLesson sends the next lesson after one is completed.
func (d *Delivery) AdvanceLesson(
	ctx context.Context,
	institutionID, learnerID, courseID int64,
) (*Notification, error) {
	next, err := d.queries.GetFirstUnansweredLesson(ctx, db.GetFirstUnansweredLessonParams{
		CourseID:      courseID,
		InstitutionID: institutionID,
		LearnerID:     learnerID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if err := d.queries.CloseSmsConversation(ctx, db.CloseSmsConversationParams{
				InstitutionID: institutionID,
				LearnerID:     learnerID,
				State:         dbState(SmsStateCompleted),
			}); err != nil {
				return nil, fmt.Errorf("sms: close conversation: %w", err)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("sms: next lesson: %w", err)
	}

	if _, err := d.queries.UpsertSmsConversation(ctx, db.UpsertSmsConversationParams{
		InstitutionID:       institutionID,
		LearnerID:           learnerID,
		State:               dbState(SmsStateAwaitingStart),
		PendingAssessmentID: nil,
		PendingQuestionID:   nil,
		PendingLessonID:     &next.ID,
		PendingPosition:     0,
		PendingTotal:        0,
		OptedOut:            false,
	}); err != nil {
		return nil, fmt.Errorf("sms: update conversation: %w", err)
	}

	lesson, err := d.queries.GetLessonForSms(ctx, db.GetLessonForSmsParams{
		ID:            next.ID,
		InstitutionID: institutionID,
	})
	if err != nil {
		return nil, fmt.Errorf("sms: load lesson: %w", err)
	}

	return &Notification{
		Body: d.templates.LessonMessage(lesson.Title, lesson.BodyMarkdown),
	}, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func (d *Delivery) closeConversation(ctx context.Context, msg OutboxMessage, state ConversationState) error {
	if err := d.queries.CloseSmsConversation(ctx, db.CloseSmsConversationParams{
		InstitutionID: msg.InstitutionID,
		LearnerID:     msg.LearnerID,
		State:         dbState(state),
	}); err != nil {
		return fmt.Errorf("sms: close conversation: %w", err)
	}
	return nil
}

// assessmentTotal counts the questions in an assessment.
func (d *Delivery) assessmentTotal(ctx context.Context, institutionID, assessmentID int64) (int, error) {
	counts, err := d.queries.CountCorrectAnswers(ctx, db.CountCorrectAnswersParams{
		AssessmentID: assessmentID,
		// Zero is a learner id that cannot exist, so every question counts as
		// unanswered and this is the plain question count.
		LearnerID: 0,
	})
	if err != nil {
		return 0, fmt.Errorf("sms: assessment size: %w", err)
	}
	return int(counts.Total), nil
}

// LoadPendingQuestion reads what a learner is currently being asked, for inbound
// reply interpretation.
func (d *Delivery) LoadPendingQuestion(ctx context.Context, institutionID, learnerID int64) (*SmsConversation, error) {
	return d.loadConversation(ctx, OutboxMessage{
		InstitutionID: institutionID,
		LearnerID:     learnerID,
	})
}
func toChoices(rows []db.ListChoicesForSmsRow) []Choice {
	out := make([]Choice, 0, len(rows))
	for _, row := range rows {
		out = append(out, Choice{Label: row.Label, Body: row.Body})
	}
	return out
}

// EncodePayload builds the notification body stored on an outbox row.
//
// Exposed so a caller can enqueue a synthetic notification -- a scheduler
// nudging a learner, say -- with the same shape the learning engine writes.
func EncodePayload(kind string, learnerID int64, occurredAt time.Time) []byte {
	raw, err := json.Marshal(map[string]any{
		"kind":        kind,
		"source":      string(learning.SourceSMS),
		"learner_id":  learnerID,
		"occurred_at": occurredAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		// Only reachable if a field here becomes unmarshalable.
		panic("sms: encode payload: " + err.Error())
	}
	return raw
}

// StripControlChars removes characters that would corrupt a message log or a
// gateway's own framing.
func StripControlChars(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
