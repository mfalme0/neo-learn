package inbound_test

import (
	"context"
	"testing"
	"time"

	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/inbound"
	"github.com/neolearn/neolearn/internal/learning"
	"github.com/neolearn/neolearn/internal/sms"
	"github.com/neolearn/neolearn/internal/testsupport"
)

func newProcessor(t *testing.T, env *testsupport.Config) (*inbound.Processor, testsupport.Fixture) {
	t.Helper()

	fx := testsupport.NewFixture(context.Background(), t, env.Pool)
	queries := db.New(env.Pool)
	templates := sms.NewTemplates()

	delivery := sms.NewDelivery(queries, templates, nil)
	processor := inbound.NewProcessor(
		queries,
		learning.NewService(env.Pool, queries),
		delivery,
		templates,
		nil,
	)

	return processor, fx
}

// openConversation makes a learner look like they have a question pending.
func openConversation(t *testing.T, env *testsupport.Config, fx testsupport.Fixture, questionID int64) {
	t.Helper()

	_, err := env.Pool.Exec(context.Background(), `
		INSERT INTO sms_conversations (institution_id, learner_id, state,
		                               pending_assessment_id, pending_question_id,
		                               pending_position, pending_total)
		VALUES ($1, $2, 'awaiting_answer', $3, $4, 0, 2)
		ON CONFLICT (institution_id, learner_id) DO UPDATE SET
			state = 'awaiting_answer',
			pending_assessment_id = EXCLUDED.pending_assessment_id,
			pending_question_id = EXCLUDED.pending_question_id,
			pending_position = EXCLUDED.pending_position,
			pending_total = EXCLUDED.pending_total,
			opted_out = false`,
		fx.InstitutionID, fx.LearnerID, fx.AssessmentID, questionID)
	if err != nil {
		t.Fatalf("open conversation: %v", err)
	}
}

func inboundMessage(msisdn, body, providerID string) sms.InboundMessage {
	return sms.InboundMessage{
		From:              msisdn,
		Body:              body,
		ProviderMessageID: providerID,
		ReceivedAt:        time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
	}
}

func countEvents(t *testing.T, env *testsupport.Config) int {
	t.Helper()

	var n int
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM learning_events WHERE learner_id = (SELECT id FROM users WHERE display_name = 'Joseph Otieno')`,
	).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// ---------------------------------------------------------------------------
// Answering
// ---------------------------------------------------------------------------

func TestAnsweringBySmsRecordsTheAnswer(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, fx.CorrectLabels[0], "gw-msg-1"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if !result.Handled {
		t.Error("Handled = false for a valid answer")
	}
	if result.Outcome != inbound.OutcomeAnswered {
		t.Errorf("Outcome = %q, want %q", result.Outcome, inbound.OutcomeAnswered)
	}

	var marksAwarded int32
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT marks_awarded FROM progress WHERE learner_id = $1 AND entity = 'assessment'`,
		fx.LearnerID).Scan(&marksAwarded); err != nil {
		t.Fatalf("read progress: %v", err)
	}
	if marksAwarded != 1 {
		t.Errorf("marks_awarded = %d, want 1", marksAwarded)
	}
}

func TestAnswerIsRecordedWithSmsSource(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	if _, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, fx.CorrectLabels[0], "gw-msg-1")); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	var source, kind string
	if err := env.Pool.QueryRow(context.Background(), `
		SELECT source::text, kind::text FROM learning_events
		WHERE question_id = $1`, fx.QuestionIDs[0]).Scan(&source, &kind); err != nil {
		t.Fatalf("read event: %v", err)
	}

	// The source is what makes an SMS answer distinguishable from the same
	// answer submitted in a browser.
	if source != "sms" {
		t.Errorf("source = %q, want sms", source)
	}
	if kind != "question_answered" {
		t.Errorf("kind = %q, want question_answered", kind)
	}
}

// The central guarantee, at the transport boundary: a learner who texts the
// same answer twice is scored once.
func TestDuplicateWebhookDoesNotDoubleScore(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	msg := inboundMessage(fx.LearnerMSISDN, fx.CorrectLabels[0], "gw-msg-dup")

	first, err := processor.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	if first.Duplicate {
		t.Error("first delivery reported Duplicate = true")
	}

	// The gateway retries the identical webhook.
	second, err := processor.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("second Handle() error = %v", err)
	}
	if !second.Duplicate {
		t.Error("retried webhook was not recognised as a duplicate")
	}

	if n := countEvents(t, env); n != 1 {
		t.Errorf("learning_events has %d rows, want 1", n)
	}

	var marksAwarded int32
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT marks_awarded FROM progress WHERE learner_id = $1 AND entity = 'assessment'`,
		fx.LearnerID).Scan(&marksAwarded); err != nil {
		t.Fatalf("read progress: %v", err)
	}
	if marksAwarded != 1 {
		t.Errorf("marks_awarded = %d after a duplicate webhook, want 1", marksAwarded)
	}
}

// A duplicate must not cost a second acknowledgement either.
func TestDuplicateWebhookSendsNoSecondReply(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])
	msg := inboundMessage(fx.LearnerMSISDN, fx.CorrectLabels[0], "gw-msg-dup")

	if _, err := processor.Handle(context.Background(), msg); err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	second, err := processor.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("second Handle() error = %v", err)
	}

	// A learner who pressed send twice already got one acknowledgement.
	if second.Reply != "" {
		t.Errorf("duplicate produced a second reply: %q", second.Reply)
	}
}

func TestAnswerIsCaseInsensitive(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	// Lowercase is what a feature phone keyboard produces.
	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, " "+lower(fx.CorrectLabels[0])+" ", "gw-msg-lower"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if result.Outcome != inbound.OutcomeAnswered {
		t.Errorf("Outcome = %q, want answered", result.Outcome)
	}
}

// The learner clock is the gateway's timestamp, which is what makes delivery
// latency measurable.
func TestAnswerUsesTheGatewayTimestamp(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	actedAt := time.Date(2026, 1, 1, 9, 10, 0, 0, time.UTC)
	msg := inboundMessage(fx.LearnerMSISDN, fx.CorrectLabels[0], "gw-msg-time")
	msg.ReceivedAt = actedAt

	if _, err := processor.Handle(context.Background(), msg); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	var occurred time.Time
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT occurred_at FROM learning_events WHERE question_id = $1`,
		fx.QuestionIDs[0]).Scan(&occurred); err != nil {
		t.Fatalf("read event: %v", err)
	}

	if !occurred.Equal(actedAt) {
		t.Errorf("occurred_at = %v, want the gateway's %v", occurred.UTC(), actedAt)
	}
}

// ---------------------------------------------------------------------------
// Unparseable and out-of-context replies
// ---------------------------------------------------------------------------

func TestAnswerNotOnOfferIsRejectedWithHelp(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "Z", "gw-msg-z"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if result.Outcome != inbound.OutcomeUnrecognized {
		t.Errorf("Outcome = %q, want unrecognized", result.Outcome)
	}
	// Echoing the input is what lets a learner see the mistake.
	if result.Reply == "" {
		t.Error("no reply for an unparseable answer; the learner would think nothing arrived")
	}

	if n := countEvents(t, env); n != 0 {
		t.Errorf("learning_events has %d rows for a rejected answer, want 0", n)
	}
}

func TestReplyWithNoPendingQuestionGetsHelp(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	// No conversation: nothing has been asked.
	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "A", "gw-msg-noconv"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	if result.Outcome != inbound.OutcomeNoPending {
		t.Errorf("Outcome = %q, want no_pending_question", result.Outcome)
	}
	if result.Reply == "" {
		t.Error("no reply; the learner cannot tell whether they were heard")
	}
	if n := countEvents(t, env); n != 0 {
		t.Errorf("learning_events has %d rows, want 0", n)
	}
}

func TestUnknownSenderIsIgnoredQuietly(t *testing.T) {
	env := testsupport.Get(t)
	processor, _ := newProcessor(t, env)

	// An unknown number must be acknowledged, not retried: the same message
	// will always be unknown.
	result, err := processor.Handle(context.Background(), inboundMessage(
		"+254700000999", "A", "gw-msg-stranger"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if result.Outcome != inbound.OutcomeUnknownSender {
		t.Errorf("Outcome = %q, want unknown_sender", result.Outcome)
	}
}

func TestUnusableSenderNumberIsIgnored(t *testing.T) {
	env := testsupport.Get(t)
	processor, _ := newProcessor(t, env)

	for _, from := range []string{"", "not-a-number", "0700000001"} {
		result, err := processor.Handle(context.Background(), inboundMessage(from, "A", "gw-msg-bad"))
		if err != nil {
			t.Fatalf("Handle(%q) error = %v", from, err)
		}
		if result.Outcome != inbound.OutcomeUnknownSender {
			t.Errorf("Handle(%q).Outcome = %q, want unknown_sender", from, result.Outcome)
		}
	}
}

// Punctuation in a stored number must not stop the reply being routed.
func TestSenderNumberIsNormalised(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	formatted := fx.LearnerMSISDN[:4] + " " + fx.LearnerMSISDN[4:7] + "-" + fx.LearnerMSISDN[7:]
	result, err := processor.Handle(context.Background(), inboundMessage(
		formatted, fx.CorrectLabels[0], "gw-msg-formatted"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if result.Outcome != inbound.OutcomeAnswered {
		t.Errorf("Outcome = %q, want answered", result.Outcome)
	}
}

// ---------------------------------------------------------------------------
// Opt-out
// ---------------------------------------------------------------------------

// A learner who texts STOP and keeps receiving messages concludes the system is
// broken.
func TestStopIsHonouredImmediately(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "STOP", "gw-msg-stop"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if result.Outcome != inbound.OutcomeOptedOut {
		t.Errorf("Outcome = %q, want opted_out", result.Outcome)
	}

	var optedOut bool
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT opted_out FROM sms_conversations WHERE learner_id = $1`, fx.LearnerID).Scan(&optedOut); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if !optedOut {
		t.Error("opted_out = false after STOP")
	}

	// The confirmation must say how to resume, or the opt-out is a dead end.
	if result.Reply == "" {
		t.Fatal("no confirmation sent for STOP")
	}
}

func TestOptOutCancelsAnyPendingQuestion(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])
	if _, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "stop", "gw-msg-stop2")); err != nil {
		t.Fatalf("Handle(STOP) error = %v", err)
	}

	var pendingQuestion *int64
	var state string
	if err := env.Pool.QueryRow(context.Background(), `
		SELECT pending_question_id, state::text FROM sms_conversations WHERE learner_id = $1`,
		fx.LearnerID).Scan(&pendingQuestion, &state); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if pendingQuestion != nil {
		t.Error("a pending question survived the opt-out")
	}
	if state != "idle" {
		t.Errorf("state = %q after STOP, want idle", state)
	}
}

func TestStartResumesMessages(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])
	if _, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "STOP", "gw-msg-stop3")); err != nil {
		t.Fatalf("Handle(STOP) error = %v", err)
	}

	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "START", "gw-msg-start"))
	if err != nil {
		t.Fatalf("Handle(START) error = %v", err)
	}
	if result.Outcome != inbound.OutcomeCommand {
		t.Errorf("Outcome = %q, want command", result.Outcome)
	}

	var optedOut bool
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT opted_out FROM sms_conversations WHERE learner_id = $1`, fx.LearnerID).Scan(&optedOut); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if optedOut {
		t.Error("opted_out = true after START")
	}
}

func TestHelpIsAnswered(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])

	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "help?", "gw-msg-help"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if result.Outcome != inbound.OutcomeCommand {
		t.Errorf("Outcome = %q, want command", result.Outcome)
	}
	if result.Reply == "" {
		t.Error("HELP produced no reply")
	}
}

// ---------------------------------------------------------------------------
// Lessons
// ---------------------------------------------------------------------------

func TestCompletingALessonBySms(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	_, err := env.Pool.Exec(context.Background(), `
		INSERT INTO sms_conversations (institution_id, learner_id, state, pending_lesson_id)
		VALUES ($1, $2, 'awaiting_start', $3)`,
		fx.InstitutionID, fx.LearnerID, fx.LessonID)
	if err != nil {
		t.Fatalf("open lesson conversation: %v", err)
	}

	result, err := processor.Handle(context.Background(), inboundMessage(
		fx.LearnerMSISDN, "DONE", "gw-msg-done"))
	if err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if result.Outcome != inbound.OutcomeAnswered {
		t.Errorf("Outcome = %q, want answered", result.Outcome)
	}

	var status string
	if err := env.Pool.QueryRow(context.Background(),
		`SELECT status FROM progress WHERE learner_id = $1 AND lesson_id = $2`,
		fx.LearnerID, fx.LessonID).Scan(&status); err != nil {
		t.Fatalf("read progress: %v", err)
	}
	if status != "completed" {
		t.Errorf("lesson status = %q, want completed", status)
	}
}

func TestCompletingALessonTwiceIsDeduplicated(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	_, err := env.Pool.Exec(context.Background(), `
		INSERT INTO sms_conversations (institution_id, learner_id, state, pending_lesson_id)
		VALUES ($1, $2, 'awaiting_start', $3)`,
		fx.InstitutionID, fx.LearnerID, fx.LessonID)
	if err != nil {
		t.Fatalf("open lesson conversation: %v", err)
	}

	msg := inboundMessage(fx.LearnerMSISDN, "DONE", "gw-msg-done-2")

	if _, err := processor.Handle(context.Background(), msg); err != nil {
		t.Fatalf("first Handle() error = %v", err)
	}
	second, err := processor.Handle(context.Background(), msg)
	if err != nil {
		t.Fatalf("second Handle() error = %v", err)
	}
	if !second.Duplicate {
		t.Error("retried lesson completion was not recognised as a duplicate")
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// Two deliveries of the same message racing each other must still record once:
// the database constraint, not the application, is what makes that true.
func TestConcurrentDuplicateDeliveriesRecordOnce(t *testing.T) {
	env := testsupport.Get(t)
	processor, fx := newProcessor(t, env)

	openConversation(t, env, fx, fx.QuestionIDs[0])
	msg := inboundMessage(fx.LearnerMSISDN, fx.CorrectLabels[0], "gw-msg-race")

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			defer func() { done <- struct{}{} }()
			//nolint:errcheck // the assertion is on the resulting row count.
			_, _ = processor.Handle(context.Background(), msg)
		}()
	}
	<-done
	<-done

	if n := countEvents(t, env); n != 1 {
		t.Errorf("learning_events has %d rows after a concurrent duplicate, want 1", n)
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
