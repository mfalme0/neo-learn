package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/neolearn/neolearn/internal/db"
)

// ---------------------------------------------------------------------------
// Courses
// ---------------------------------------------------------------------------

type courseSummary struct {
	ID          int64  `json:"id"`
	Code        string `json:"code"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

type courseDetail struct {
	courseSummary
	Published bool `json:"published"`
}

func (s *Server) handleListCourses(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	rows, err := s.queries.ListEnrolledCourses(r.Context(), db.ListEnrolledCoursesParams{
		LearnerID:     identity.UserID,
		InstitutionID: identity.InstitutionID,
	})
	if err != nil {
		internalError(w, r, "courses.list", err)
		return
	}

	out := make([]courseSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, courseSummary{
			ID:          row.ID,
			Code:        row.Code,
			Title:       row.Title,
			Description: row.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"courses": out})
}

func (s *Server) handleGetCourse(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	courseID, err := chiParamInt64(r, "courseID")
	if err != nil {
		writeRequestError(w, err)
		return
	}

	course, err := s.queries.GetCourse(r.Context(), db.GetCourseParams{
		CourseID:      courseID,
		InstitutionID: identity.InstitutionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "course_not_found", "no such course")
			return
		}
		internalError(w, r, "courses.get", err)
		return
	}

	if !s.learnerEnrolled(r, identity.LearnerID(), courseID) {
		writeError(w, http.StatusNotFound, "course_not_found", "no such course")
		return
	}

	writeJSON(w, http.StatusOK, courseDetail{
		courseSummary: courseSummary{
			ID:          course.ID,
			Code:        course.Code,
			Title:       course.Title,
			Description: course.Description,
		},
		Published: course.Published,
	})
}

// ---------------------------------------------------------------------------
// Lessons
// ---------------------------------------------------------------------------

type lessonSummary struct {
	ID          int64  `json:"id"`
	ModuleID    int64  `json:"module_id"`
	Title       string `json:"title"`
	SMSEligible bool   `json:"sms_eligible"`
	Status      string `json:"status"`
	Percent     int32  `json:"percent"`
	HasLesson   bool   `json:"has_lesson"`
}

type moduleView struct {
	ID      int64           `json:"id"`
	Title   string          `json:"title"`
	Lessons []lessonSummary `json:"lessons"`
}

type lessonDetail struct {
	ID           int64         `json:"id"`
	ModuleID     int64         `json:"module_id"`
	CourseID     int64         `json:"course_id"`
	Title        string        `json:"title"`
	BodyMarkdown string        `json:"body_markdown"`
	SMSEligible  bool          `json:"sms_eligible"`
	Progress     *progressView `json:"progress,omitempty"`
}

type progressView struct {
	Status            string     `json:"status"`
	QuestionsAnswered int32      `json:"questions_answered"`
	QuestionsCorrect  int32      `json:"questions_correct"`
	MarksAwarded      int32      `json:"marks_awarded"`
	MarksAvailable    int32      `json:"marks_available"`
	Percent           int32      `json:"percent"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
}

func (s *Server) handleListLessons(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	courseID, err := chiParamInt64(r, "courseID")
	if err != nil {
		writeRequestError(w, err)
		return
	}

	rows, err := s.queries.ListModulesWithLessons(r.Context(), db.ListModulesWithLessonsParams{
		CourseID:      courseID,
		InstitutionID: identity.InstitutionID,
		LearnerID:     identity.LearnerID(),
	})
	if err != nil {
		internalError(w, r, "lessons.list", err)
		return
	}
	if len(rows) == 0 {
		writeError(w, http.StatusNotFound, "course_not_found", "no such course")
		return
	}

	// Flat rows to nested modules. A module with no lessons still appears,
	// with an empty slice, so the client does not have to distinguish
	// "empty module" from "module missing".
	modules := make([]moduleView, 0, 4)
	index := make(map[int64]int, 4)

	for _, row := range rows {
		pos, seen := index[row.ModuleID]
		if !seen {
			modules = append(modules, moduleView{
				ID:      row.ModuleID,
				Title:   row.ModuleTitle,
				Lessons: make([]lessonSummary, 0, 4),
			})
			pos = len(modules) - 1
			index[row.ModuleID] = pos
		}

		// A module with no lessons still yields a row, with all lesson
		// columns NULL. Those are skipped so an empty module renders as an
		// empty list rather than a phantom lesson.
		if row.LessonID == nil {
			continue
		}
		modules[pos].Lessons = append(modules[pos].Lessons, lessonSummary{
			ID:          *row.LessonID,
			ModuleID:    row.ModuleID,
			Title:       *row.LessonTitle,
			SMSEligible: *row.SmsEligible,
			Status:      row.ProgressStatus,
			Percent:     row.Percent,
			HasLesson:   true,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"modules": modules})
}

func (s *Server) handleGetLesson(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	lessonID, err := chiParamInt64(r, "lessonID")
	if err != nil {
		writeRequestError(w, err)
		return
	}

	lesson, err := s.queries.GetLesson(r.Context(), db.GetLessonParams{
		LessonID:      lessonID,
		InstitutionID: identity.InstitutionID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "lesson_not_found", "no such lesson")
			return
		}
		internalError(w, r, "lessons.get", err)
		return
	}

	if !s.learnerEnrolled(r, identity.LearnerID(), lesson.CourseID) {
		writeError(w, http.StatusNotFound, "lesson_not_found", "no such lesson")
		return
	}

	detail := lessonDetail{
		ID:           lesson.ID,
		ModuleID:     lesson.ModuleID,
		CourseID:     lesson.CourseID,
		Title:        lesson.Title,
		BodyMarkdown: lesson.BodyMarkdown,
		SMSEligible:  lesson.SmsEligible,
	}

	if p, err := s.queries.GetLessonProgress(r.Context(), db.GetLessonProgressParams{
		LearnerID:     identity.LearnerID(),
		LessonID:      &lessonID,
		InstitutionID: identity.InstitutionID,
	}); err == nil {
		detail.Progress = &progressView{
			Status:      p.Status,
			Percent:     p.Percent,
			StartedAt:   pgTime(p.StartedAt),
			CompletedAt: pgTime(p.CompletedAt),
		}
	}

	writeJSON(w, http.StatusOK, detail)
}

// ---------------------------------------------------------------------------
// Assessments
// ---------------------------------------------------------------------------

type assessmentSummary struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	SMSEligible  bool   `json:"sms_eligible"`
	Status       string `json:"status"`
	Percent      int32  `json:"percent"`
	MarksAwarded int32  `json:"marks_awarded"`
	MarksTotal   int32  `json:"marks_available"`
}

type questionView struct {
	ID       int64        `json:"id"`
	Kind     string       `json:"kind"`
	Prompt   string       `json:"prompt"`
	Marks    int32        `json:"marks"`
	Position int32        `json:"position"`
	Choices  []choiceView `json:"choices"`
	// AnsweredLabel is what the learner last submitted, if anything.
	AnsweredLabel string `json:"answered_label,omitempty"`
}

type choiceView struct {
	Label string `json:"label"`
	Body  string `json:"body"`
}

type assessmentDetail struct {
	ID          int64          `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	SMSEligible bool           `json:"sms_eligible"`
	Questions   []questionView `json:"questions"`
	Progress    *progressView  `json:"progress,omitempty"`
}

func (s *Server) handleListAssessments(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	courseID, err := chiParamInt64(r, "courseID")
	if err != nil {
		writeRequestError(w, err)
		return
	}

	if !s.learnerEnrolled(r, identity.LearnerID(), courseID) {
		writeError(w, http.StatusNotFound, "course_not_found", "no such course")
		return
	}

	rows, err := s.queries.ListAssessmentsForCourse(r.Context(), db.ListAssessmentsForCourseParams{
		CourseID:      courseID,
		InstitutionID: identity.InstitutionID,
		LearnerID:     identity.LearnerID(),
	})
	if err != nil {
		internalError(w, r, "assessments.list", err)
		return
	}

	out := make([]assessmentSummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, assessmentSummary{
			ID:           row.ID,
			Title:        row.Title,
			Description:  row.Description,
			SMSEligible:  row.SmsEligible,
			Status:       row.ProgressStatus,
			Percent:      row.Percent,
			MarksAwarded: row.MarksAwarded,
			MarksTotal:   row.MarksAvailable,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"assessments": out})
}

func (s *Server) handleGetAssessment(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	assessmentID, err := chiParamInt64(r, "assessmentID")
	if err != nil {
		writeRequestError(w, err)
		return
	}

	rows, err := s.queries.GetAssessmentWithQuestions(r.Context(), db.GetAssessmentWithQuestionsParams{
		AssessmentID:  assessmentID,
		InstitutionID: identity.InstitutionID,
	})
	if err != nil {
		internalError(w, r, "assessments.get", err)
		return
	}
	if len(rows) == 0 {
		writeError(w, http.StatusNotFound, "assessment_not_found", "no such assessment")
		return
	}

	choices, err := s.queries.ListChoicesForAssessment(r.Context(), db.ListChoicesForAssessmentParams{
		AssessmentID:  assessmentID,
		InstitutionID: identity.InstitutionID,
	})
	if err != nil {
		internalError(w, r, "assessments.choices", err)
		return
	}

	// Group choices by question. is_correct is deliberately dropped: a
	// learner must not be able to read the answer key out of the payload
	// before submitting.
	byQuestion := make(map[int64][]choiceView)
	for _, c := range choices {
		byQuestion[c.QuestionID] = append(byQuestion[c.QuestionID], choiceView{
			Label: c.Label,
			Body:  c.Body,
		})
	}

	detail := assessmentDetail{
		ID:          rows[0].AssessmentID,
		Title:       rows[0].AssessmentTitle,
		Description: rows[0].AssessmentDescription,
		SMSEligible: rows[0].SmsEligible,
		Questions:   make([]questionView, 0, len(rows)),
	}

	for _, row := range rows {
		// An assessment with no questions yields one row with NULL question
		// columns; that is not a question and is skipped.
		if row.QuestionID == nil {
			continue
		}
		q := questionView{
			ID:       *row.QuestionID,
			Kind:     string(*row.QuestionKind),
			Prompt:   *row.Prompt,
			Marks:    *row.Marks,
			Position: *row.QuestionPosition,
			Choices:  byQuestion[*row.QuestionID],
		}
		if q.Choices == nil {
			q.Choices = []choiceView{}
		}
		if answered, err := s.queries.GetLearnerAnswer(r.Context(), db.GetLearnerAnswerParams{
			LearnerID:     identity.LearnerID(),
			QuestionID:    &q.ID,
			InstitutionID: identity.InstitutionID,
		}); err == nil && answered.ChoiceLabel != nil {
			// The column is a JSONB extraction, so its type is unknown to
			// codegen and arrives as any. Normalise to a string here.
			if label, ok := answered.ChoiceLabel.(string); ok {
				q.AnsweredLabel = label
			}
		}
		detail.Questions = append(detail.Questions, q)
	}

	if p, err := s.queries.GetAssessmentProgress(r.Context(), db.GetAssessmentProgressParams{
		LearnerID:     identity.LearnerID(),
		AssessmentID:  &assessmentID,
		InstitutionID: identity.InstitutionID,
	}); err == nil {
		detail.Progress = &progressView{
			Status:            p.Status,
			QuestionsAnswered: p.QuestionsAnswered,
			QuestionsCorrect:  p.QuestionsCorrect,
			MarksAwarded:      p.MarksAwarded,
			MarksAvailable:    p.MarksAvailable,
			Percent:           p.Percent,
			StartedAt:         pgTime(p.StartedAt),
			CompletedAt:       pgTime(p.CompletedAt),
		}
	}

	writeJSON(w, http.StatusOK, detail)
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

func (s *Server) handleGetProgress(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	rows, err := s.queries.GetCourseProgress(r.Context(), db.GetCourseProgressParams{
		LearnerID:     identity.LearnerID(),
		InstitutionID: identity.InstitutionID,
	})
	if err != nil {
		internalError(w, r, "progress.by_course", err)
		return
	}

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

	out := make([]courseProgress, 0, len(rows))
	for _, row := range rows {
		percent := int32(0)
		if row.MarksAvailable > 0 {
			percent = min(100, (row.MarksAwarded*100)/row.MarksAvailable)
		}
		out = append(out, courseProgress{
			CourseID:         row.CourseID,
			Code:             row.Code,
			Title:            row.Title,
			LessonsCompleted: row.LessonsCompleted,
			LessonsTotal:     row.LessonsTotal,
			MarksAwarded:     row.MarksAwarded,
			MarksAvailable:   row.MarksAvailable,
			Percent:          percent,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"courses": out})
}

// learnerEnrolled reports whether the learner is enrolled in a course.
//
// A missing enrolment is reported as "no such course" by callers rather than
// 403: a learner should not be able to probe which course ids exist.
func (s *Server) learnerEnrolled(r *http.Request, learnerID, courseID int64) bool {
	ok, err := s.queries.IsEnrolled(r.Context(), db.IsEnrolledParams{
		CourseID:  courseID,
		LearnerID: learnerID,
	})
	if err != nil {
		// Fail closed. Authorising on an error would let a database blip
		// turn into a data leak.
		return false
	}
	return ok
}
