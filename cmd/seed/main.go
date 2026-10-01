// Command seed populates a development database with a realistic learning
// world: one institution, a teacher, two learners, a published course with
// lessons and a graded assessment.
//
// It is idempotent in the sense that it refuses to run against a database that
// already has courses, rather than silently duplicating content. Local
// development should start from a known state, not accumulate seeds.
//
// The password for every seeded account is printed on completion.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/neolearn/neolearn/internal/auth"
	"github.com/neolearn/neolearn/internal/config"
	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/store"
)

const seedPassword = "learner-password-123"

func main() {
	force := flag.Bool("force", false, "delete existing courses and users before seeding")
	flag.Parse()

	if err := run(*force); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run(force bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	queries := db.New(pool.Pool)

	// Count institutions, not courses: a partially seeded run that failed
	// before creating a course would otherwise be re-seeded on top of itself.
	existing, err := queries.CountInstitutions(ctx)
	if err != nil {
		return fmt.Errorf("check for existing data: %w", err)
	}
	if existing > 0 && !force {
		return fmt.Errorf("database already has %d institution(s); re-run with --force to reset", existing)
	}
	if force {
		// Order matters only for readability; the foreign keys cascade.
		for _, table := range []string{
			"outbox", "progress", "learning_events", "course_enrollments",
			"choices", "questions", "assessments", "lessons", "modules", "courses", "users",
		} {
			if _, err := pool.Exec(ctx, "DELETE FROM "+table); err != nil {
				return fmt.Errorf("clear %s: %w", table, err)
			}
		}
		if _, err := pool.Exec(ctx, "DELETE FROM institutions"); err != nil {
			return fmt.Errorf("clear institutions: %w", err)
		}
		fmt.Println("cleared existing data")
	}

	hasher := auth.DefaultHashParams(cfg.Argon2.Time, cfg.Argon2.Memory, cfg.Argon2.Threads)
	passwordHash, err := hasher.Hash(seedPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	// One transaction: a partially seeded database is worse than none.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := queries.WithTx(tx)

	summary, err := seed(ctx, q, passwordHash)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	printSummary(summary)
	return nil
}

type summary struct {
	InstitutionID   int64
	InstitutionSlug string
	TeacherID       int64
	TeacherEmail    string
	LearnerEmails   []string
	CourseID        int64
	CourseCode      string
	LessonIDs       []int64
	AssessmentID    int64
	QuestionCount   int
}

func seed(ctx context.Context, q *db.Queries, passwordHash string) (summary, error) {
	var s summary

	inst, err := q.CreateInstitution(ctx, db.CreateInstitutionParams{
		Slug: "kibera-secondary",
		Name: "Kibera Secondary School",
	})
	if err != nil {
		return s, fmt.Errorf("create institution: %w", err)
	}
	s.InstitutionID = inst.ID
	s.InstitutionSlug = inst.Slug

	teacher, err := q.CreateUser(ctx, db.CreateUserParams{
		InstitutionID: inst.ID,
		Email:         "grace.teacher@example.test",
		DisplayName:   "Grace Mwangi",
		Role:          db.UserRoleTeacher,
		PasswordHash:  &passwordHash,
	})
	if err != nil {
		return s, fmt.Errorf("create teacher: %w", err)
	}
	s.TeacherID = teacher.ID
	s.TeacherEmail = teacher.Email

	// Two learners. The first has a phone number because the SMS path in
	// milestone 2 needs one to deliver to.
	learners := []struct {
		email  string
		name   string
		msisdn string
	}{
		{"joseph.learner@example.test", "Joseph Otieno", "+254700000001"},
		{"amina.learner@example.test", "Amina Hassan", ""},
	}

	learnerIDs := make([]int64, 0, len(learners))
	for _, l := range learners {
		params := db.CreateUserParams{
			InstitutionID: inst.ID,
			Email:         l.email,
			DisplayName:   l.name,
			Role:          db.UserRoleStudent,
			PasswordHash:  &passwordHash,
		}
		if l.msisdn != "" {
			params.Msisdn = &l.msisdn
		}

		created, err := q.CreateUser(ctx, params)
		if err != nil {
			return s, fmt.Errorf("create learner %s: %w", l.email, err)
		}
		learnerIDs = append(learnerIDs, created.ID)
		s.LearnerEmails = append(s.LearnerEmails, created.Email)
	}

	description := "Number, algebra, and geometry for the second term."
	course, err := q.CreateCourse(ctx, db.CreateCourseParams{
		InstitutionID: inst.ID,
		Code:          "MATH101",
		Title:         "Mathematics",
		Description:   &description,
		Published:     true,
	})
	if err != nil {
		return s, fmt.Errorf("create course: %w", err)
	}
	s.CourseID = course.ID
	s.CourseCode = course.Code

	// Only the first learner is enrolled, so /v1/courses has something to
	// scope to and the second account shows an empty state.
	enrolledBy := teacher.ID
	if _, err := q.EnrolLearner(ctx, db.EnrolLearnerParams{
		CourseID:      course.ID,
		LearnerID:     learnerIDs[0],
		InstitutionID: inst.ID,
		AssignedBy:    &enrolledBy,
	}); err != nil {
		return s, fmt.Errorf("enrol learner: %w", err)
	}

	module, err := q.CreateModule(ctx, db.CreateModuleParams{
		CourseID:      course.ID,
		InstitutionID: inst.ID,
		Title:         "Algebra",
		Position:      0,
	})
	if err != nil {
		return s, fmt.Errorf("create module: %w", err)
	}

	lessons := []struct {
		title       string
		body        string
		smsEligible bool
	}{
		{
			title: "Linear Equations",
			body: "A linear equation has exactly one unknown. To solve one, " +
				"isolate the unknown on one side of the equals sign.\n\n" +
				"Example: 2x + 4 = 10. Subtract 4 from both sides to get 2x = 6, " +
				"then divide by 2 to get x = 3.",
			smsEligible: true,
		},
		{
			title: "Fractions",
			body: "A fraction represents part of a whole. The number above the " +
				"line is the numerator; the number below is the denominator.\n\n" +
				"To compare two fractions with the same denominator, compare the " +
				"numerators: 1/2 is larger than 1/4.",
			// Long prose with paragraph breaks does not chunk cleanly into SMS
			// messages, so this one is not eligible for SMS delivery.
			smsEligible: false,
		},
	}

	for i, l := range lessons {
		body := l.body
		created, err := q.CreateLesson(ctx, db.CreateLessonParams{
			ModuleID:      module.ID,
			InstitutionID: inst.ID,
			Title:         l.title,
			BodyMarkdown:  &body,
			SmsEligible:   l.smsEligible,
			Position:      int32(i),
		})
		if err != nil {
			return s, fmt.Errorf("create lesson %q: %w", l.title, err)
		}
		s.LessonIDs = append(s.LessonIDs, created.ID)
	}

	assessmentDescription := "Two questions on linear equations."
	assessment, err := q.CreateAssessment(ctx, db.CreateAssessmentParams{
		CourseID:      course.ID,
		InstitutionID: inst.ID,
		Title:         "Algebra 01",
		Description:   &assessmentDescription,
		Published:     true,
		SmsEligible:   true,
		Position:      0,
	})
	if err != nil {
		return s, fmt.Errorf("create assessment: %w", err)
	}
	s.AssessmentID = assessment.ID

	questions := []struct {
		prompt  string
		correct string
	}{
		{"Solve for x: 2x + 4 = 10", "B"},
		{"Solve for x: 3x - 6 = 9", "A"},
		{"Solve for x: 5x = 25", "C"},
	}

	for i, question := range questions {
		created, err := q.CreateQuestion(ctx, db.CreateQuestionParams{
			AssessmentID:  assessment.ID,
			InstitutionID: inst.ID,
			Kind:          db.QuestionKindSingleChoice,
			Prompt:        question.prompt,
			Marks:         1,
			Position:      int32(i),
		})
		if err != nil {
			return s, fmt.Errorf("create question %d: %w", i, err)
		}
		s.QuestionCount++

		// Four choices, one correct. Single-letter labels because a learner
		// has to be able to answer this by texting a single character.
		answers := []string{"3", "5", "3", "5"}
		if i == 1 {
			answers = []string{"5", "3", "15", "27"}
		}
		if i == 2 {
			answers = []string{"3", "4", "5", "30"}
		}

		for pos := range 4 {
			label := string(rune('A' + pos))
			if _, err := q.CreateChoice(ctx, db.CreateChoiceParams{
				QuestionID:    created.ID,
				InstitutionID: inst.ID,
				Label:         label,
				Body:          answers[pos],
				IsCorrect:     label == question.correct,
				Position:      int32(pos),
			}); err != nil {
				return s, fmt.Errorf("create choice %d for question %d: %w", pos, i, err)
			}
		}

		// The schema cannot express "exactly one correct" without also
		// forbidding zero during authoring, so it is verified here.
		counts, err := q.CountChoicesForQuestion(ctx, created.ID)
		if err != nil {
			return s, fmt.Errorf("verify choices for question %d: %w", i, err)
		}
		if counts.Total != 4 {
			return s, fmt.Errorf("question %d has %d choices, want 4", i, counts.Total)
		}
		if counts.Correct != 1 {
			return s, fmt.Errorf("question %d has %d correct choices, want exactly 1", i, counts.Correct)
		}
	}

	return s, nil
}

func printSummary(s summary) {
	var b strings.Builder

	b.WriteString("\n")
	b.WriteString("Seeded Neo Learn development data.\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "  institution   %s (id=%d)\n", s.InstitutionSlug, s.InstitutionID)
	fmt.Fprintf(&b, "  course        %s (id=%d)\n", s.CourseCode, s.CourseID)
	fmt.Fprintf(&b, "  lessons       %d\n", len(s.LessonIDs))
	fmt.Fprintf(&b, "  assessment    id=%d, %d questions\n", s.AssessmentID, s.QuestionCount)
	b.WriteString("\n")
	b.WriteString("  Sign in at http://localhost:3000/login with:\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "    institution  %s\n", s.InstitutionSlug)
	fmt.Fprintf(&b, "    teacher      %s\n", s.TeacherEmail)
	for _, email := range s.LearnerEmails {
		fmt.Fprintf(&b, "    learner      %s\n", email)
	}
	fmt.Fprintf(&b, "    password     %s\n", seedPassword)
	b.WriteString("\n")

	fmt.Print(b.String())
}
