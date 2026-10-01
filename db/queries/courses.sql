-- name: ListEnrolledCourses :many
SELECT
    c.id,
    c.code,
    c.title,
    c.description
FROM courses c
JOIN course_enrollments ce
  ON ce.course_id = c.id
 AND ce.learner_id = @learner_id
WHERE c.institution_id = @institution_id
  AND c.published = true
ORDER BY c.title;

-- name: GetCourse :one
SELECT
    c.id,
    c.code,
    c.title,
    c.description,
    c.published
FROM courses c
WHERE c.id = @course_id
  AND c.institution_id = @institution_id;

-- name: ListModulesWithLessons :many
-- Returns one flat, ordered list; the service groups it in memory. A flat
-- query avoids the row-multiplication a nested rows.Cursor would produce
-- across the modules/lessons cross product.
SELECT
    m.id                AS module_id,
    m.title             AS module_title,
    m.position          AS module_position,
    l.id                AS lesson_id,
    l.title             AS lesson_title,
    l.sms_eligible      AS sms_eligible,
    l.position          AS lesson_position,
    COALESCE(p.status, 'not_started') AS progress_status,
    COALESCE(p.percent, 0)           AS percent
FROM modules m
LEFT JOIN lessons l
  ON l.module_id = m.id
LEFT JOIN progress p
  ON p.lesson_id = l.id
 AND p.learner_id = @learner_id
WHERE m.course_id = @course_id
  AND m.institution_id = @institution_id
ORDER BY m.position, l.position;

-- name: GetLesson :one
SELECT
    l.id,
    l.module_id,
    l.title,
    l.body_markdown,
    l.sms_eligible,
    l.position,
    m.course_id
FROM lessons l
JOIN modules m ON m.id = l.module_id
WHERE l.id = @lesson_id
  AND l.institution_id = @institution_id;

-- name: GetLessonProgress :one
SELECT
    status,
    questions_answered,
    questions_correct,
    marks_awarded,
    marks_available,
    percent,
    started_at,
    completed_at
FROM progress
WHERE learner_id = @learner_id
  AND lesson_id = @lesson_id
  AND institution_id = @institution_id;

-- name: ListAssessmentsForCourse :many
SELECT
    a.id,
    a.title,
    a.description,
    a.position,
    a.sms_eligible,
    COALESCE(p.status, 'not_started') AS progress_status,
    COALESCE(p.percent, 0)           AS percent,
    COALESCE(p.marks_awarded, 0)     AS marks_awarded,
    COALESCE(p.marks_available, 0)   AS marks_available
FROM assessments a
LEFT JOIN progress p
  ON p.assessment_id = a.id
 AND p.learner_id = @learner_id
WHERE a.course_id = @course_id
  AND a.institution_id = @institution_id
  AND a.published = true
ORDER BY a.position;

-- name: GetAssessmentWithQuestions :many
SELECT
    a.id            AS assessment_id,
    a.title         AS assessment_title,
    a.description   AS assessment_description,
    a.sms_eligible  AS sms_eligible,
    q.id            AS question_id,
    q.kind          AS question_kind,
    q.prompt        AS prompt,
    q.marks         AS marks,
    q.position      AS question_position
FROM assessments a
LEFT JOIN questions q ON q.assessment_id = a.id
WHERE a.id = @assessment_id
  AND a.institution_id = @institution_id
ORDER BY q.position;

-- name: ListChoicesForAssessment :many
-- is_correct is projected but the HTTP layer must never send it to a learner
-- before submission. See httpapi.writeProgress.
SELECT
    ch.id,
    ch.question_id,
    ch.label,
    ch.body,
    ch.is_correct,
    ch.position
FROM choices ch
JOIN questions q ON q.id = ch.question_id
WHERE q.assessment_id = @assessment_id
  AND ch.institution_id = @institution_id
ORDER BY ch.question_id, ch.position;

-- name: GetAssessmentProgress :one
SELECT
    status,
    questions_answered,
    questions_correct,
    marks_awarded,
    marks_available,
    percent,
    started_at,
    completed_at
FROM progress
WHERE learner_id = @learner_id
  AND assessment_id = @assessment_id
  AND institution_id = @institution_id;

-- name: GetLearnerAnswer :one
-- Which label the learner submitted for a question, used to render a
-- previously-answered state.
SELECT
    payload ->> 'choice_label' AS choice_label,
    received_at
FROM learning_events
WHERE learner_id = @learner_id
  AND question_id = @question_id
  AND institution_id = @institution_id
  AND kind = 'question_answered'
ORDER BY received_at DESC
LIMIT 1;