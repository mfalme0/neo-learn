package httpapi_test

import (
	"bytes"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/neolearn/neolearn/internal/testsupport"
)

// The API-level counterpart to the engine's idempotency test: a client that
// retries a submit -- because it never saw the response -- must not be scored
// twice. This is what an offline queue does on reconnect.
func TestSubmittingTheSameEventTwiceScoresOnce(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	eventID := uuid.NewString()
	body := map[string]any{
		"event_id":     eventID,
		"kind":         "question_answered",
		"question_id":  h.Fixture.QuestionIDs[0],
		"choice_label": h.Fixture.CorrectLabels[0],
	}

	first := postEvent(t, client, h, body)
	if first.Duplicate {
		t.Error("first submit reported duplicate = true")
	}
	if first.Graded == nil {
		t.Fatal("first submit returned no grading")
	}
	if !first.Graded.Correct {
		t.Error("correct answer graded as incorrect")
	}
	if first.Graded.MarksAwarded != 1 {
		t.Errorf("marks_awarded = %d, want 1", first.Graded.MarksAwarded)
	}

	// Retry, byte-identical.
	second := postEvent(t, client, h, body)
	if !second.Duplicate {
		t.Error("retry of an identical event reported duplicate = false")
	}

	// One mark awarded out of two available -- not two marks, which is what
	// double-counting would produce.
	progress := getProgress(t, client, h)
	if progress.MarksAwarded != 1 {
		t.Errorf("marks_awarded = %d, want 1", progress.MarksAwarded)
	}
	if progress.MarksAvailable != 2 {
		t.Errorf("marks_available = %d, want 2", progress.MarksAvailable)
	}
	if progress.Percent != 50 {
		t.Errorf("percent = %d, want 50", progress.Percent)
	}

	// And only one row in the log.
	var events int
	if err := h.Env.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM learning_events WHERE event_id = $1`, eventID).Scan(&events); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events != 1 {
		t.Errorf("learning_events has %d rows for one event_id, want 1", events)
	}
}

// A learner cannot answer on someone else's behalf. The learner id comes from
// the session, never from the body, so there is no field to tamper with.
func TestEventCannotTargetAnotherLearner(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	// An explicit learner_id must be rejected as an unknown field rather than
	// silently ignored, so the caller learns it is not doing what they think.
	resp, err := client.Post(h.Server.URL+"/v1/events", "application/json", jsonBody(map[string]any{
		"event_id":     uuid.NewString(),
		"kind":         "question_answered",
		"question_id":  h.Fixture.QuestionIDs[0],
		"choice_label": "A",
		"learner_id":   h.Fixture.TeacherID,
	}))
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d for an event carrying learner_id, want 400", resp.StatusCode)
	}
}

// The answer key must never reach a learner before they answer. Otherwise the
// web UI can be bypassed entirely by reading the payload.
func TestAssessmentPayloadDoesNotLeakTheAnswerKey(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	resp, err := client.Get(h.Server.URL + "/v1/assessments/" + itoa(h.Fixture.AssessmentID))
	if err != nil {
		t.Fatalf("get assessment: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	raw := readBody(t, resp)
	for _, forbidden := range []string{"is_correct", "isCorrect", "correct"} {
		if bytes.Contains([]byte(raw), []byte(forbidden)) {
			t.Errorf("assessment payload contains %q:\n%s", forbidden, raw)
		}
	}

	// The answer key is still server-side and the submission is still graded.
	eventID := uuid.NewString()
	graded := postEvent(t, client, h, map[string]any{
		"event_id":     eventID,
		"kind":         "question_answered",
		"question_id":  h.Fixture.QuestionIDs[0],
		"choice_label": h.Fixture.CorrectLabels[0],
	})
	if graded.Graded == nil || !graded.Graded.Correct {
		t.Error("submitting the correct label was not graded correct")
	}
}

// An answer for a question outside the learner's institution must not be
// accepted, and must not confirm whether the question exists.
func TestEventForForeignQuestionIsRejected(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	otherInstitution, _, otherCourse := testsupport.OtherInstitution(t.Context(), t, h.Env.Pool)

	// The assessment hangs off the other institution's own course, so the
	// whole chain is internally consistent the way real content would be.
	// Asserting on the returned ids keeps a broken fixture from masquerading
	// as a passing authorisation test.
	var foreignAssessment int64
	if err := h.Env.Pool.QueryRow(t.Context(), `
		INSERT INTO assessments (course_id, institution_id, title, published, position)
		VALUES ($1, $2, 'Foreign Assessment', true, 0)
		RETURNING id`, otherCourse, otherInstitution).Scan(&foreignAssessment); err != nil {
		t.Fatalf("create foreign assessment (course=%d institution=%d): %v",
			otherCourse, otherInstitution, err)
	}

	var foreignQuestion int64
	if err := h.Env.Pool.QueryRow(t.Context(), `
		INSERT INTO questions (assessment_id, institution_id, kind, prompt, marks, position)
		VALUES ($1, $2, 'single_choice', 'Foreign question', 1, 0)
		RETURNING id`,
		foreignAssessment, otherInstitution).Scan(&foreignQuestion); err != nil {
		t.Fatalf("create foreign question: %v", err)
	}

	// A matching choice, so the rejection cannot be attributed to a missing
	// choice rather than to the tenant boundary.
	if _, err := h.Env.Pool.Exec(t.Context(), `
		INSERT INTO choices (question_id, institution_id, label, body, is_correct, position)
		VALUES ($1, $2, 'A', 'answer A', true, 0)`,
		foreignQuestion, otherInstitution); err != nil {
		t.Fatalf("create foreign choice: %v", err)
	}

	resp, err := client.Post(h.Server.URL+"/v1/events", "application/json", jsonBody(map[string]any{
		"event_id":     uuid.NewString(),
		"kind":         "question_answered",
		"question_id":  foreignQuestion,
		"choice_label": "A",
	}))
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		t.Error("answering a question in another institution returned 200")
	}
}

// A learner must not be able to read another learner's progress.
func TestProgressIsScopedToTheAuthenticatedLearner(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	// Create a second learner in the same institution with their own progress.
	var otherLearner int64
	if err := h.Env.Pool.QueryRow(t.Context(), `
		INSERT INTO users (institution_id, email, display_name, role)
		VALUES ($1, 'second.learner@example.test', 'Second Learner', 'student')
		RETURNING id`, h.Fixture.InstitutionID).Scan(&otherLearner); err != nil {
		t.Fatalf("create second learner: %v", err)
	}
	if _, err := h.Env.Pool.Exec(t.Context(), `
		INSERT INTO course_enrollments (course_id, learner_id, institution_id)
		VALUES ($1, $2, $3)`, h.Fixture.CourseID, otherLearner, h.Fixture.InstitutionID); err != nil {
		t.Fatalf("enroll second learner: %v", err)
	}
	if _, err := h.Env.Pool.Exec(t.Context(), `
		INSERT INTO progress (institution_id, learner_id, course_id, assessment_id, entity, status,
			marks_awarded, marks_available, questions_answered, questions_correct, percent, started_at)
		VALUES ($1, $2, $3, $4, 'assessment', 'in_progress', 2, 2, 2, 2, 100, now())`,
		h.Fixture.InstitutionID, otherLearner, h.Fixture.CourseID, h.Fixture.AssessmentID); err != nil {
		t.Fatalf("insert other learner progress: %v", err)
	}

	progress := getProgress(t, client, h)
	// The authenticated learner has answered nothing; the other learner's 2/2
	// must not appear.
	if progress.MarksAwarded != 0 {
		t.Errorf("marks_awarded = %d, want 0 (another learner's progress leaked)", progress.MarksAwarded)
	}
	if progress.MarksAvailable != 0 {
		t.Errorf("marks_available = %d, want 0 (another learner's progress leaked)", progress.MarksAvailable)
	}
}

// Invalid input must be rejected with a usable message, not a stack trace.
func TestInvalidEventRequestsAreRejected(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	cases := []struct {
		name     string
		body     map[string]any
		wantCode int
	}{
		{
			name:     "missing event_id",
			body:     map[string]any{"kind": "lesson_started", "lesson_id": h.Fixture.LessonID},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "event_id is not a uuid",
			body:     map[string]any{"event_id": "abc", "kind": "lesson_started", "lesson_id": h.Fixture.LessonID},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "unknown kind",
			body:     map[string]any{"event_id": uuid.NewString(), "kind": "teleported", "lesson_id": h.Fixture.LessonID},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "answer with no choice",
			body:     map[string]any{"event_id": uuid.NewString(), "kind": "question_answered", "question_id": h.Fixture.QuestionIDs[0]},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "answer with no question",
			body:     map[string]any{"event_id": uuid.NewString(), "kind": "question_answered", "choice_label": "A"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "choice that does not exist",
			body:     map[string]any{"event_id": uuid.NewString(), "kind": "question_answered", "question_id": h.Fixture.QuestionIDs[0], "choice_label": "Z"},
			wantCode: http.StatusNotFound,
		},
		{
			name:     "lesson event with no lesson",
			body:     map[string]any{"event_id": uuid.NewString(), "kind": "lesson_started"},
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "unknown source",
			body:     map[string]any{"event_id": uuid.NewString(), "kind": "lesson_started", "lesson_id": h.Fixture.LessonID, "source": "carrier_pigeon"},
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.Post(h.Server.URL+"/v1/events", "application/json", jsonBody(tc.body))
			if err != nil {
				t.Fatalf("post event: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.wantCode {
				t.Errorf("status = %d, want %d (body: %s)", resp.StatusCode, tc.wantCode, readBody(t, resp))
			}

			var body errorBody
			decodeJSONBody(t, resp, &body)
			if body.Code == "" {
				t.Error("error response has no machine-readable code")
			}
			if body.Error == "" {
				t.Error("error response has no message")
			}
		})
	}
}

// A learner answering "b" in lowercase, as an SMS reply might arrive, must be
// graded the same as "B".
func TestChoiceLabelIsCaseInsensitive(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	graded := postEvent(t, client, h, map[string]any{
		"event_id":     uuid.NewString(),
		"kind":         "question_answered",
		"question_id":  h.Fixture.QuestionIDs[0],
		"choice_label": " b ",
	})
	if graded.Graded == nil {
		t.Fatal("no grading returned")
	}
	if !graded.Graded.Correct {
		t.Errorf(`lowercase " b " graded as incorrect; want correct (label %q)`,
			h.Fixture.CorrectLabels[0])
	}
}

// A path parameter that is not a number must be a 400, not a 500 from a
// failed scan.
func TestInvalidPathParametersReturn400(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	for _, path := range []string{
		"/v1/courses/abc",
		"/v1/courses/0",
		"/v1/courses/-1",
		"/v1/lessons/not-a-number",
		"/v1/assessments/999999999999999999999999",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := client.Get(h.Server.URL + path)
			if err != nil {
				t.Fatalf("get %s: %v", path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// A malformed body must not produce a 500.
func TestMalformedJSONIsRejected(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	for _, raw := range []string{
		``,
		`{`,
		`not json at all`,
		`{"event_id": }`,
		`{"event_id":"a","event_id":"b"}`,
		`{}{}`,
	} {
		t.Run(raw, func(t *testing.T) {
			resp, err := client.Post(h.Server.URL+"/v1/events", "application/json", bytes.NewReader([]byte(raw)))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d for %q, want 400", resp.StatusCode, raw)
			}
		})
	}
}

// The health endpoint must stay reachable without a session, or a load
// balancer cannot probe it.
func TestHealthEndpointIsPublic(t *testing.T) {
	h := newHarness(t)

	resp, err := h.client(t).Get(h.Server.URL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// Completing a lesson through the API must move the projection, which is what
// the dashboard and the learner's future self will read.
func TestLessonCompletionUpdatesProgress(t *testing.T) {
	h := newHarness(t)
	client := h.login(t)

	eventID := uuid.NewString()
	resp, err := client.Post(h.Server.URL+"/v1/events", "application/json", jsonBody(map[string]any{
		"event_id":  eventID,
		"kind":      "lesson_completed",
		"lesson_id": h.Fixture.LessonID,
		"source":    "sms",
	}))
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, readBody(t, resp))
	}

	lessonResp, err := client.Get(h.Server.URL + "/v1/lessons/" + itoa(h.Fixture.LessonID))
	if err != nil {
		t.Fatalf("get lesson: %v", err)
	}
	defer func() { _ = lessonResp.Body.Close() }()

	var lesson struct {
		Progress *struct {
			Status string `json:"status"`
		} `json:"progress"`
	}
	decodeJSONBody(t, lessonResp, &lesson)

	if lesson.Progress == nil {
		t.Fatal("lesson has no progress after being completed")
	}
	if lesson.Progress.Status != "completed" {
		t.Errorf("status = %q, want completed", lesson.Progress.Status)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type eventResponse struct {
	EventID   string `json:"event_id"`
	Duplicate bool   `json:"duplicate"`
	Graded    *struct {
		QuestionID   int64  `json:"question_id"`
		ChoiceLabel  string `json:"choice_label"`
		Correct      bool   `json:"correct"`
		Marks        int32  `json:"marks"`
		MarksAwarded int32  `json:"marks_awarded"`
		MarksTotal   int32  `json:"marks_total"`
		Percent      int32  `json:"percent"`
	} `json:"graded"`
}

func postEvent(t *testing.T, client *http.Client, h *harness, body map[string]any) eventResponse {
	t.Helper()

	resp, err := client.Post(h.Server.URL+"/v1/events", "application/json", jsonBody(body))
	if err != nil {
		t.Fatalf("post event: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, readBody(t, resp))
	}

	var out eventResponse
	decodeJSONBody(t, resp, &out)
	return out
}

// courseProgress is one row of GET /v1/progress.
type courseProgress struct {
	CourseID         int64  `json:"course_id"`
	Code             string `json:"code"`
	Title            string `json:"title"`
	LessonsCompleted int64  `json:"lessons_completed"`
	LessonsTotal     int64  `json:"lessons_total"`
	MarksAwarded     int32  `json:"marks_awarded"`
	MarksAvailable   int32  `json:"marks_available"`
	Percent          int32  `json:"percent"`
}

// getProgress returns the authenticated learner's progress for the seeded
// course, failing if it is absent.
func getProgress(t *testing.T, client *http.Client, h *harness) courseProgress {
	t.Helper()

	resp, err := client.Get(h.Server.URL + "/v1/progress")
	if err != nil {
		t.Fatalf("get progress: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out struct {
		Courses []courseProgress `json:"courses"`
	}
	decodeJSONBody(t, resp, &out)

	for _, c := range out.Courses {
		if c.CourseID == h.Fixture.CourseID {
			return c
		}
	}
	t.Fatal("progress did not include the learner's enrolled course")
	return courseProgress{}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
