package testsupport

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/neolearn/neolearn/internal/auth"
)

// Fixture is a small but complete learning world: an institution, a teacher,
// a learner enrolled in a course, a lesson, and an assessment with two
// single-choice questions.
//
// Every test that exercises the learning engine needs this shape. Building it
// in one place keeps the tests focused on behaviour rather than fixture setup.
type Fixture struct {
	InstitutionID int64
	TeacherID     int64
	LearnerID     int64
	LearnerMSISDN string
	Password      string

	CourseID     int64
	ModuleID     int64
	LessonID     int64
	AssessmentID int64
	// QuestionIDs has one entry per question, in position order.
	QuestionIDs []int64
	// CorrectLabels[i] is the correct choice label for QuestionIDs[i].
	CorrectLabels []string
}

const (
	// The fixture uses deliberately cheap argon2 params: a real hash costs
	// ~50ms and the suite creates one per test. Nothing in the learning
	// engine depends on the cost, and auth.DefaultHashParams/Verify tests
	// cover the real parameter handling.
	fixtureArgonTime   = 1
	fixtureArgonMemory = 8 * 1024
	fixtureArgonThread = 1
)

// FixturePassword is the password for every seeded account.
const FixturePassword = "learner-password-123"

// NewFixture truncates the database and builds a fresh learning world.
func NewFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) Fixture {
	t.Helper()

	Truncate(ctx, t, pool)

	hasher := auth.DefaultHashParams(fixtureArgonTime, fixtureArgonMemory, fixtureArgonThread)
	passwordHash, err := hasher.Hash(FixturePassword)
	if err != nil {
		t.Fatalf("hash fixture password: %v", err)
	}

	f := Fixture{Password: FixturePassword}

	if err := pool.QueryRow(ctx, `
		INSERT INTO institutions (slug, name)
		VALUES ('kibera-secondary', 'Kibera Secondary School')
		RETURNING id`).Scan(&f.InstitutionID); err != nil {
		t.Fatalf("create institution: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO users (institution_id, email, display_name, role, password_hash)
		VALUES ($1, 'grace.teacher@example.test', 'Grace Mwangi', 'teacher', $2)
		RETURNING id`, f.InstitutionID, passwordHash).Scan(&f.TeacherID); err != nil {
		t.Fatalf("create teacher: %v", err)
	}

	// An E.164 number, so the outbox row gets a delivery destination.
	f.LearnerMSISDN = "+254700000001"
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (institution_id, email, display_name, role, password_hash, msisdn)
		VALUES ($1, 'joseph.learner@example.test', 'Joseph Otieno', 'student', $2, $3)
		RETURNING id`, f.InstitutionID, passwordHash, f.LearnerMSISDN).Scan(&f.LearnerID); err != nil {
		t.Fatalf("create learner: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO courses (institution_id, code, title, description, published)
		VALUES ($1, 'MATH101', 'Mathematics', 'Number, algebra, and geometry.', true)
		RETURNING id`, f.InstitutionID).Scan(&f.CourseID); err != nil {
		t.Fatalf("create course: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO course_enrollments (course_id, learner_id, institution_id, assigned_by)
		VALUES ($1, $2, $3, $4)`,
		f.CourseID, f.LearnerID, f.InstitutionID, f.TeacherID); err != nil {
		t.Fatalf("enroll learner: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO modules (course_id, institution_id, title, position)
		VALUES ($1, $2, 'Algebra', 0)
		RETURNING id`, f.CourseID, f.InstitutionID).Scan(&f.ModuleID); err != nil {
		t.Fatalf("create module: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO lessons (module_id, institution_id, title, body_markdown, sms_eligible, position)
		VALUES ($1, $2, 'Linear Equations', 'A linear equation has one unknown.', true, 0)
		RETURNING id`, f.ModuleID, f.InstitutionID).Scan(&f.LessonID); err != nil {
		t.Fatalf("create lesson: %v", err)
	}

	if err := pool.QueryRow(ctx, `
		INSERT INTO assessments (course_id, institution_id, title, description, published, sms_eligible, position)
		VALUES ($1, $2, 'Algebra 01', 'Two questions on linear equations.', true, true, 0)
		RETURNING id`, f.CourseID, f.InstitutionID).Scan(&f.AssessmentID); err != nil {
		t.Fatalf("create assessment: %v", err)
	}

	questions := []struct {
		prompt  string
		correct string
	}{
		{"Solve for x: 2x + 4 = 10", "B"},
		{"Solve for x: 3x - 6 = 9", "A"},
	}

	for i, q := range questions {
		var questionID int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO questions (assessment_id, institution_id, kind, prompt, marks, position)
			VALUES ($1, $2, 'single_choice', $3, 1, $4)
			RETURNING id`,
			f.AssessmentID, f.InstitutionID, q.prompt, i).Scan(&questionID); err != nil {
			t.Fatalf("create question %d: %v", i, err)
		}
		f.QuestionIDs = append(f.QuestionIDs, questionID)

		// Four choices, exactly one correct.
		for pos := range 4 {
			label := string(rune('A' + pos))
			if _, err := pool.Exec(ctx, `
				INSERT INTO choices (question_id, institution_id, label, body, is_correct, position)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				questionID, f.InstitutionID, label,
				"answer "+label, label == q.correct, pos); err != nil {
				t.Fatalf("create choice %d for question %d: %v", pos, i, err)
			}
		}
		f.CorrectLabels = append(f.CorrectLabels, q.correct)
	}

	return f
}

// OtherInstitution inserts a second institution with one learner enrolled in
// its own course. Used to prove that institution scoping actually holds.
func OtherInstitution(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (institutionID, learnerID, courseID int64) {
	t.Helper()

	hasher := auth.DefaultHashParams(fixtureArgonTime, fixtureArgonMemory, fixtureArgonThread)
	hash, err := hasher.Hash(FixturePassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO institutions (slug, name)
		VALUES ('other-school', 'Other School')
		RETURNING id`).Scan(&institutionID)
	if err != nil {
		t.Fatalf("create institution: %v", err)
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO users (institution_id, email, display_name, role, password_hash)
		VALUES ($1, 'outsider@example.test', 'Outsider', 'student', $2)
		RETURNING id`, institutionID, hash).Scan(&learnerID)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO courses (institution_id, code, title, published)
		VALUES ($1, 'OTHER1', 'Other Course', true)
		RETURNING id`, institutionID).Scan(&courseID)
	if err != nil {
		t.Fatalf("create course: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO course_enrollments (course_id, learner_id, institution_id)
		VALUES ($1, $2, $3)`, courseID, learnerID, institutionID); err != nil {
		t.Fatalf("enroll: %v", err)
	}

	return institutionID, learnerID, courseID
}
