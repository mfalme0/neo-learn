-- Authoring queries.
--
-- Not exercised by the API in milestone 1: there is no teacher interface yet.
-- They exist so the schema can be exercised end-to-end by the seed command and
-- so the next milestone adds endpoints rather than starting from scratch.

-- name: CreateInstitution :one
INSERT INTO institutions (slug, name)
VALUES ($1, $2)
RETURNING id, slug, name;

-- name: CreateUser :one
INSERT INTO users (institution_id, email, display_name, role, password_hash, msisdn)
VALUES ($1, $2, $3, $4, sqlc.narg('password_hash')::text, sqlc.narg('msisdn')::text)
RETURNING id, institution_id, email, display_name, role, msisdn;

-- name: CreateCourse :one
INSERT INTO courses (institution_id, code, title, description, published)
VALUES ($1, $2, $3, sqlc.narg('description')::text, sqlc.arg('published')::boolean)
RETURNING id, code, title, published;

-- name: CreateModule :one
INSERT INTO modules (course_id, institution_id, title, position)
VALUES ($1, $2, $3, $4)
RETURNING id, title, position;

-- name: CreateLesson :one
INSERT INTO lessons (module_id, institution_id, title, body_markdown, sms_eligible, position)
VALUES ($1, $2, $3, sqlc.narg('body_markdown')::text, sqlc.arg('sms_eligible')::boolean, $4)
RETURNING id, title, sms_eligible, position;

-- name: CreateAssessment :one
INSERT INTO assessments (course_id, institution_id, title, description, published, sms_eligible, position)
VALUES ($1, $2, $3, sqlc.narg('description')::text,
        sqlc.arg('published')::boolean, sqlc.arg('sms_eligible')::boolean, $4)
RETURNING id, title, published, sms_eligible;

-- name: CreateQuestion :one
INSERT INTO questions (assessment_id, institution_id, kind, prompt, marks, position)
VALUES ($1, $2, sqlc.arg('kind')::question_kind, $3, sqlc.arg('marks')::int, $4)
RETURNING id, prompt, marks, position;

-- name: CreateChoice :one
INSERT INTO choices (question_id, institution_id, label, body, is_correct, position)
VALUES ($1, $2, $3, $4, sqlc.arg('is_correct')::boolean, $5)
RETURNING id, label, body, is_correct, position;

-- name: EnrolLearner :one
INSERT INTO course_enrollments (course_id, learner_id, institution_id, assigned_by)
VALUES ($1, $2, $3, sqlc.narg('assigned_by')::bigint)
ON CONFLICT (course_id, learner_id) DO NOTHING
RETURNING id;

-- name: CountCourses :one
SELECT count(*) FROM courses WHERE institution_id = $1;

-- name: CountInstitutions :one
SELECT count(*) FROM institutions;

-- name: CountChoicesForQuestion :one
-- Used by the seed command to assert the "exactly one correct answer" rule
-- that a partial unique index cannot express.
SELECT
    count(*)                          AS total,
    count(*) FILTER (WHERE is_correct) AS correct
FROM choices
WHERE question_id = $1;