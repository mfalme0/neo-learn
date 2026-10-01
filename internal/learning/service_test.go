package learning_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/learning"
	"github.com/neolearn/neolearn/internal/testsupport"
)

func newService(t *testing.T, ctx context.Context) (*learning.Service, testsupport.Fixture) {
	t.Helper()

	env := testsupport.Get(t)
	fx := testsupport.NewFixture(ctx, t, env.Pool)

	return learning.NewService(env.Pool, db.New(env.Pool)), fx
}

func answerEvent(fx testsupport.Fixture, label string) learning.Event {
	return learning.Event{
		EventID:       uuid.New(),
		InstitutionID: fx.InstitutionID,
		LearnerID:     fx.LearnerID,
		QuestionID:    ptr(fx.QuestionIDs[0]),
		Kind:          learning.KindQuestionAnswered,
		Source:        learning.SourceWeb,
		Payload:       mustPayload(learning.AnswerPayload{ChoiceLabel: label}),
		OccurredAt:    time.Now().UTC(),
	}
}

func ptr[T any](v T) *T { return &v }

func mustPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// The central guarantee of the whole design: submitting the same answer twice
// scores once. This is the case the README calls out -- a learner texts "B"
// because they did not see an acknowledgement.
func TestIngestIsIdempotentOnEventID(t *testing.T) {
	ctx := context.Background()
	svc, fx := newService(t, ctx)

	ev := answerEvent(fx, fx.CorrectLabels[0])

	first, err := svc.Ingest(ctx, ev)
	if err != nil {
		t.Fatalf("first Ingest() error = %v", err)
	}
	if first.Duplicate {
		t.Error("first Ingest() reported Duplicate = true, want false")
	}

	// Same event, same event_id: a retry.
	second, err := svc.Ingest(ctx, ev)
	if err != nil {
		t.Fatalf("retry Ingest() error = %v", err)
	}
	if !second.Duplicate {
		t.Error("retry Ingest() reported Duplicate = false, want true")
	}

	var events int
	if err := env(t).Pool.QueryRow(ctx,
		`SELECT count(*) FROM learning_events`).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("learning_events has %d rows, want 1", events)
	}

	var marksAwarded, questionsAnswered int32
	if err := env(t).Pool.QueryRow(ctx, `
		SELECT marks_awarded, questions_answered FROM progress
		WHERE learner_id = $1 AND entity = 'assessment'`,
		fx.LearnerID).Scan(&marksAwarded, &questionsAnswered); err != nil {
		t.Fatalf("read progress: %v", err)
	}
	if marksAwarded != 1 {
		t.Errorf("marks_awarded = %d after a duplicate submit, want 1", marksAwarded)
	}
	if questionsAnswered != 1 {
		t.Errorf("questions_answered = %d after a duplicate submit, want 1", questionsAnswered)
	}
}

// Two different event IDs for the same question is a re-answer, not a
// duplicate: the learner changed their mind. The projection must reflect the
// latest answer and must not double-count the question.
func TestReansweringWithNewEventIDUpdatesRatherThanAccumulates(t *testing.T) {
	ctx := context.Background()
	svc, fx := newService(t, ctx)

	if _, err := svc.Ingest(ctx, answerEvent(fx, "A")); err != nil {
		t.Fatalf("first answer error = %v", err)
	}

	// Same question, different event_id, now correct.
	second := answerEvent(fx, fx.CorrectLabels[0])
	if _, err := svc.Ingest(ctx, second); err != nil {
		t.Fatalf("second answer error = %v", err)
	}

	var marksAwarded, questionsAnswered, questionsCorrect int32
	if err := env(t).Pool.QueryRow(ctx, `
		SELECT marks_awarded, questions_answered, questions_correct FROM progress
		WHERE learner_id = $1 AND entity = 'assessment'`,
		fx.LearnerID).Scan(&marksAwarded, &questionsAnswered, &questionsCorrect); err != nil {
		t.Fatalf("read progress: %v", err)
	}

	// Two questions exist in total, so a single correct answer is 1 of 2.
	if marksAwarded != 1 {
		t.Errorf("marks_awarded = %d, want 1", marksAwarded)
	}
	if questionsAnswered != 1 {
		t.Errorf("questions_answered = %d, want 1 (distinct questions, not events)", questionsAnswered)
	}
	if questionsCorrect != 1 {
		t.Errorf("questions_correct = %d, want 1", questionsCorrect)
	}
}

// The offline queue replays events in whatever order the network delivered
// them. Both orders must converge on the same projection.
// The offline queue replays events in whatever order the network delivered
// them. Both orders must converge on the same projection.
//
// Note the two questions are deliberately given the same set of labels in
// each run so the only variable is arrival order.
func TestOutOfOrderReplayConverges(t *testing.T) {
	ctx := context.Background()

	// Forward: Q1 correct, then Q2 incorrect.
	svcA, fxA := newService(t, ctx)

	firstA := answerEvent(fxA, fxA.CorrectLabels[0])
	if _, err := svcA.Ingest(ctx, firstA); err != nil {
		t.Fatalf("Q1 error = %v", err)
	}
	secondA := answerEvent(fxA, "A")
	secondA.QuestionID = ptr(fxA.QuestionIDs[1])
	if _, err := svcA.Ingest(ctx, secondA); err != nil {
		t.Fatalf("Q2 error = %v", err)
	}
	stateA := readAssessmentState(t, ctx, fxA)

	// Reversed: Q2 first, then Q1.
	envB := testsupport.Get(t)
	fxB := testsupport.NewFixture(ctx, t, envB.Pool)
	svcB := learning.NewService(envB.Pool, db.New(envB.Pool))

	firstB := answerEvent(fxB, "A")
	firstB.QuestionID = ptr(fxB.QuestionIDs[1])
	if _, err := svcB.Ingest(ctx, firstB); err != nil {
		t.Fatalf("Q2 error = %v", err)
	}
	secondB := answerEvent(fxB, fxB.CorrectLabels[0])
	if _, err := svcB.Ingest(ctx, secondB); err != nil {
		t.Fatalf("Q1 error = %v", err)
	}
	stateB := readAssessmentState(t, ctx, fxB)

	if stateA != stateB {
		t.Errorf("out-of-order replay diverged:\n  forward  = %+v\n  reversed = %+v", stateA, stateB)
	}

	// Both runs answered the same labels to the same questions, so the
	// expected state is known independently of ordering.
	if stateA.QuestionsAnswered != 2 {
		t.Errorf("questions_answered = %d, want 2", stateA.QuestionsAnswered)
	}
}

// A stale event arriving after a newer one for the same question must not
// overwrite it. This is distinct from idempotency: the event IDs differ, so
// both are legitimately recorded, but only the latest answer should count.
func TestStaleEventDoesNotOverwriteNewerAnswer(t *testing.T) {
	ctx := context.Background()
	svc, fx := newService(t, ctx)

	// The learner answers correctly, then changes their mind.
	if _, err := svc.Ingest(ctx, answerEvent(fx, fx.CorrectLabels[0])); err != nil {
		t.Fatalf("first answer error = %v", err)
	}
	if _, err := svc.Ingest(ctx, answerEvent(fx, "A")); err != nil {
		t.Fatalf("re-answer error = %v", err)
	}

	// A late-arriving event carrying the OLD label, with an occurred_at that
	// predates both. This is the shape an offline queue produces.
	stale := answerEvent(fx, fx.CorrectLabels[0])
	stale.OccurredAt = time.Now().UTC().Add(-2 * time.Hour)
	if _, err := svc.Ingest(ctx, stale); err != nil {
		t.Fatalf("stale answer error = %v", err)
	}

	state := readAssessmentState(t, ctx, fx)
	if state.QuestionsCorrect != 0 {
		t.Errorf("questions_correct = %d after a stale event overwrote a newer answer, want 0", state.QuestionsCorrect)
	}
	if state.QuestionsAnswered != 1 {
		t.Errorf("questions_answered = %d, want 1 (one question, three events)", state.QuestionsAnswered)
	}

	// The log still holds all three events: history is append-only even
	// though only the latest is projected.
	var logged int
	if err := env(t).Pool.QueryRow(ctx,
		`SELECT count(*) FROM learning_events WHERE learner_id = $1`, fx.LearnerID).Scan(&logged); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if logged != 3 {
		t.Errorf("learning_events has %d rows, want 3", logged)
	}
}

// Completion is sticky. A stale 'lesson_started' arriving after a
// 'lesson_completed' must not reopen the lesson -- otherwise an offline replay
// would make completed work look incomplete.
func TestLessonCompletionIsStickyAgainstLateStartEvent(t *testing.T) {
	ctx := context.Background()
	svc, fx := newService(t, ctx)

	completed := learning.Event{
		EventID:       uuid.New(),
		InstitutionID: fx.InstitutionID,
		LearnerID:     fx.LearnerID,
		LessonID:      ptr(fx.LessonID),
		Kind:          learning.KindLessonCompleted,
		Source:        learning.SourceSMS,
		OccurredAt:    time.Now().UTC(),
	}
	if _, err := svc.Ingest(ctx, completed); err != nil {
		t.Fatalf("complete error = %v", err)
	}

	// An older event that only just arrived.
	staleStart := completed
	staleStart.EventID = uuid.New()
	staleStart.Kind = learning.KindLessonStarted
	staleStart.OccurredAt = completed.OccurredAt.Add(-time.Hour)
	if _, err := svc.Ingest(ctx, staleStart); err != nil {
		t.Fatalf("late start error = %v", err)
	}

	var status string
	if err := env(t).Pool.QueryRow(ctx,
		`SELECT status FROM progress WHERE learner_id = $1 AND lesson_id = $2`,
		fx.LearnerID, fx.LessonID).Scan(&status); err != nil {
		t.Fatalf("read lesson progress: %v", err)
	}
	if status != "completed" {
		t.Errorf("status = %q after a late start event, want \"completed\"", status)
	}
}

// The event log records when the learner acted as well as when the server saw
// it. The gap is offline lag and must survive the round trip.
func TestOccurredAtIsPreservedAsLearnerClock(t *testing.T) {
	ctx := context.Background()
	svc, fx := newService(t, ctx)

	actedAt := time.Now().UTC().Add(-6 * time.Hour)
	ev := answerEvent(fx, fx.CorrectLabels[0])
	ev.OccurredAt = actedAt

	if _, err := svc.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	// Postgres timestamptz stores microsecond precision, so compare at that
	// granularity. Without the truncation a nanosecond-carrying Go time would
	// never compare equal to what came back.
	const micros = time.Microsecond
	actedAt = actedAt.Truncate(micros)

	var occurred, received time.Time
	if err := env(t).Pool.QueryRow(ctx,
		`SELECT occurred_at, received_at FROM learning_events WHERE event_id = $1`,
		ev.EventID).Scan(&occurred, &received); err != nil {
		t.Fatalf("read event: %v", err)
	}

	if !occurred.Equal(actedAt) {
		t.Errorf("occurred_at = %v, want %v", occurred.UTC(), actedAt)
	}
	if lag := received.Sub(occurred); lag < 5*time.Hour {
		t.Errorf("received_at - occurred_at = %v, want roughly 6h to reflect offline lag", lag)
	}
}

// The outbox row is written in the same transaction as the event. Nothing
// drains it until milestone 2, but it must exist now so ingest does not have
// to be rewritten when the SMS worker lands.
func TestOutboxRowIsWrittenWithTheEvent(t *testing.T) {
	ctx := context.Background()
	svc, fx := newService(t, ctx)

	ev := answerEvent(fx, fx.CorrectLabels[0])
	if _, err := svc.Ingest(ctx, ev); err != nil {
		t.Fatalf("Ingest() error = %v", err)
	}

	var topic, destination string
	if err := env(t).Pool.QueryRow(ctx, `
		SELECT topic, destination_msisdn FROM outbox WHERE event_id = $1`,
		ev.EventID).Scan(&topic, &destination); err != nil {
		t.Fatalf("read outbox: %v", err)
	}

	if topic != "learning.question_answered" {
		t.Errorf("topic = %q, want learning.question_answered", topic)
	}
	if destination != fx.LearnerMSISDN {
		t.Errorf("destination_msisdn = %q, want %q", destination, fx.LearnerMSISDN)
	}
}

// A duplicate must not enqueue a second notification, or a learner who texts
// twice gets two acknowledgements.
func TestDuplicateDoesNotEnqueueSecondNotification(t *testing.T) {
	ctx := context.Background()
	svc, fx := newService(t, ctx)

	ev := answerEvent(fx, fx.CorrectLabels[0])
	if _, err := svc.Ingest(ctx, ev); err != nil {
		t.Fatalf("first Ingest() error = %v", err)
	}
	if _, err := svc.Ingest(ctx, ev); err != nil {
		t.Fatalf("duplicate Ingest() error = %v", err)
	}

	var count int
	if err := env(t).Pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE event_id = $1`, ev.EventID).Scan(&count); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if count != 1 {
		t.Errorf("outbox has %d rows for one event, want 1", count)
	}
}

// The same client-generated UUID submitted by a different learner must not
// collide with the first. This guards against a client that derives IDs
// predictably rather than randomly.
func TestEventIDIsGloballyUniqueNotPerLearner(t *testing.T) {
	ctx := context.Background()
	env := testsupport.Get(t)
	fx := testsupport.NewFixture(ctx, t, env.Pool)
	svc := learning.NewService(env.Pool, db.New(env.Pool))

	otherInstitution, otherLearner, otherCourse := testsupport.OtherInstitution(ctx, t, env.Pool)

	shared := uuid.New()

	first := learning.Event{
		EventID:       shared,
		InstitutionID: fx.InstitutionID,
		LearnerID:     fx.LearnerID,
		QuestionID:    ptr(fx.QuestionIDs[0]),
		Kind:          learning.KindQuestionAnswered,
		Source:        learning.SourceWeb,
		Payload:       mustPayload(learning.AnswerPayload{ChoiceLabel: "A"}),
		OccurredAt:    time.Now().UTC(),
	}
	if _, err := svc.Ingest(ctx, first); err != nil {
		t.Fatalf("first Ingest() error = %v", err)
	}

	// The second learner reuses the same UUID. This should be rejected as a
	// duplicate rather than silently credited to the wrong learner.
	second := first
	second.InstitutionID = otherInstitution
	second.LearnerID = otherLearner
	second.QuestionID = nil
	second.Kind = learning.KindLessonStarted
	second.LessonID = nil
	second.AssessmentID = &otherCourse
	second.Payload = []byte("{}")

	result, err := svc.Ingest(ctx, second)
	if err == nil && !result.Duplicate {
		t.Error("reusing another learner's event_id was accepted as a new event")
	}
}

type assessmentState struct {
	QuestionsAnswered int32
	QuestionsCorrect  int32
	MarksAwarded      int32
	MarksAvailable    int32
	Percent           int32
}

func readAssessmentState(t *testing.T, ctx context.Context, fx testsupport.Fixture) assessmentState {
	t.Helper()

	var s assessmentState
	err := env(t).Pool.QueryRow(ctx, `
		SELECT questions_answered, questions_correct, marks_awarded, marks_available, percent
		FROM progress WHERE learner_id = $1 AND entity = 'assessment'`,
		fx.LearnerID).Scan(&s.QuestionsAnswered, &s.QuestionsCorrect, &s.MarksAwarded, &s.MarksAvailable, &s.Percent)
	if err != nil {
		t.Fatalf("read assessment state: %v", err)
	}
	return s
}

// env returns the shared test environment.
func env(t *testing.T) *testsupport.Config {
	t.Helper()
	return testsupport.Get(t)
}
