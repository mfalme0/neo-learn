-- Learning event ingest and progress projection.
--
-- The idempotency guarantee lives in InsertLearningEvent's ON CONFLICT
-- DO NOTHING. InsertEvent returns the row only when it was actually written,
-- so the caller can skip projection work for a duplicate -- which is what
-- makes a learner who texts the same answer twice score it once.

-- name: InsertLearningEvent :one
INSERT INTO learning_events (
    event_id,
    institution_id,
    learner_id,
    course_id,
    lesson_id,
    assessment_id,
    question_id,
    kind,
    source,
    payload,
    occurred_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (event_id) DO NOTHING
RETURNING id, event_id, received_at;

-- name: GetChoiceForQuestion :one
-- Returns the choice matching a label, used to grade a submitted answer.
SELECT
    id,
    question_id,
    label,
    body,
    is_correct
FROM choices
WHERE question_id = $1
  AND institution_id = $2
  AND label = $3;

-- name: GetQuestionMarks :one
SELECT
    q.id,
    q.assessment_id,
    q.marks,
    a.course_id,
    a.institution_id
FROM questions q
JOIN assessments a ON a.id = q.assessment_id
WHERE q.id = $1
  AND q.institution_id = $2;

-- name: UpsertLessonProgress :one
-- Ensures a progress row exists and returns it.
--
-- institution_id and course_id are derived from the lesson rather than
-- supplied by the caller: a client must not be able to record progress
-- against a course it does not own, and the learner should not have to know
-- the course id to complete a lesson.
INSERT INTO progress (
    institution_id,
    learner_id,
    course_id,
    lesson_id,
    entity,
    status,
    started_at,
    completed_at
)
SELECT
    l.institution_id,
    @learner_id,
    m.course_id,
    l.id,
    'lesson',
    @status,
    CASE WHEN @status = 'in_progress' THEN now() ELSE NULL END,
    CASE WHEN @status = 'completed' THEN now() ELSE NULL END
FROM lessons l
JOIN modules m ON m.id = l.module_id
WHERE l.id = @lesson_id
  AND l.institution_id = @institution_id
  AND EXISTS (
      -- The learner must be enrolled in the course the lesson belongs to.
      SELECT 1
      FROM course_enrollments ce
      WHERE ce.course_id = m.course_id
        AND ce.learner_id = @learner_id
  )
ON CONFLICT (learner_id, entity, COALESCE(lesson_id, 0), COALESCE(assessment_id, 0))
DO UPDATE SET
    status = CASE
        WHEN progress.status = 'completed' THEN 'completed'
        ELSE EXCLUDED.status
    END,
    -- Completion is sticky: once a lesson is done, a later stale event must
    -- not move it back to in_progress. Out-of-order offline replays make
    -- this a real case, not a theoretical one.
    completed_at = COALESCE(progress.completed_at, EXCLUDED.completed_at),
    started_at = COALESCE(progress.started_at, EXCLUDED.started_at),
    updated_at = now()
RETURNING
    id,
    status,
    questions_answered,
    questions_correct,
    marks_awarded,
    marks_available,
    percent,
    started_at,
    completed_at;

-- name: ApplyAnswerToProgress :one
-- Applies one graded answer to an assessment's running totals.
--
-- Guarded against double counting: the caller only reaches this query when
-- InsertLearningEvent actually inserted a row, so a replayed event never gets
-- here twice.
--
-- The correct count is recomputed from distinct answered questions rather than
-- incremented, which keeps the projection consistent even if a learner
-- re-answers the same question through a second device.
INSERT INTO progress (
    institution_id,
    learner_id,
    course_id,
    assessment_id,
    entity,
    status,
    marks_awarded,
    marks_available,
    questions_answered,
    questions_correct
)
SELECT
    a.institution_id,
    @learner_id,
    a.course_id,
    a.id,
    'assessment',
    'in_progress',
    COALESCE(scored.marks_awarded, 0),
    (SELECT COALESCE(SUM(q.marks), 0) FROM questions q WHERE q.assessment_id = a.id),
    COALESCE(scored.answered, 0),
    COALESCE(scored.correct, 0)
FROM assessments a
LEFT JOIN LATERAL (
    SELECT
        COUNT(*) FILTER (WHERE qc.is_correct)                        AS correct,
        COUNT(DISTINCT ev.question_id)                              AS answered,
        COALESCE(SUM(q.marks) FILTER (WHERE qc.is_correct), 0)      AS marks_awarded
    FROM learning_events ev
    JOIN questions qc ON qc.id = ev.question_id
    JOIN choices ch ON ch.question_id = ev.question_id
                    AND ch.label = ev.payload ->> 'choice_label'
    WHERE ev.learner_id = @learner_id
      AND ev.question_id IN (SELECT id FROM questions WHERE assessment_id = a.id)
      AND ev.kind = 'question_answered'
) scored ON true
WHERE a.id = @assessment_id
  AND a.institution_id = @institution_id
  AND EXISTS (
      SELECT 1
      FROM course_enrollments ce
      WHERE ce.course_id = a.course_id
        AND ce.learner_id = @learner_id
  )
ON CONFLICT (learner_id, entity, COALESCE(lesson_id, 0), COALESCE(assessment_id, 0))
DO UPDATE SET
    status = 'in_progress',
    marks_awarded = EXCLUDED.marks_awarded,
    marks_available = EXCLUDED.marks_available,
    questions_answered = EXCLUDED.questions_answered,
    questions_correct = EXCLUDED.questions_correct,
    percent = CASE
        WHEN EXCLUDED.marks_available = 0 THEN 0
        ELSE LEAST(100, (EXCLUDED.marks_awarded * 100) / EXCLUDED.marks_available)
    END,
    started_at = COALESCE(progress.started_at, now()),
    updated_at = now()
RETURNING
    id,
    status,
    questions_answered,
    questions_correct,
    marks_awarded,
    marks_available,
    percent;

-- name: EnqueueOutbox :one
-- Written in the same transaction as the event, so state and notification
-- cannot diverge. Nothing reads this table until milestone 2.
INSERT INTO outbox (institution_id, event_id, topic, payload, destination_msisdn)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: GetLearnerMSISDN :one
SELECT msisdn
FROM users
WHERE id = $1
  AND institution_id = $2
  AND msisdn IS NOT NULL;

-- name: IsEnrolled :one
SELECT EXISTS (
    SELECT 1
    FROM course_enrollments
    WHERE course_id = @course_id
      AND learner_id = @learner_id
);

-- name: GetCourseProgress :many
-- Lesson totals and mark totals are computed in independent LATERAL
-- subqueries on purpose.
--
-- Joining lessons and assessment-progress rows into one FROM clause multiplies
-- rows: with three lessons and three assessments, each lesson row would carry
-- the assessment totals three times and the SUM would triple. Subqueries keep
-- each total independent.
SELECT
    c.id           AS course_id,
    c.code,
    c.title,
    COALESCE(lesson_counts.total, 0)::bigint       AS lessons_total,
    COALESCE(lesson_counts.completed, 0)::bigint   AS lessons_completed,
    COALESCE(mark_counts.awarded, 0)::int         AS marks_awarded,
    COALESCE(mark_counts.available, 0)::int       AS marks_available
FROM courses c
JOIN course_enrollments ce
  ON ce.course_id = c.id
 AND ce.learner_id = @learner_id
LEFT JOIN LATERAL (
    SELECT
        COUNT(l.id) FILTER (WHERE p.status = 'completed') AS completed,
        COUNT(l.id)                                        AS total
    FROM modules m
    JOIN lessons l ON l.module_id = m.id
    LEFT JOIN progress p
      ON p.lesson_id = l.id
     AND p.learner_id = @learner_id
    WHERE m.course_id = c.id
) lesson_counts ON true
LEFT JOIN LATERAL (
    -- SUM over INT columns yields BIGINT while the 0 fallback is INTEGER, so
    -- both branches are cast to INT. Without this the result type is
    -- untyped "any" and codegen gives up, producing interface{}.
    SELECT
        COALESCE(SUM(p.marks_awarded)::int, 0)   AS awarded,
        COALESCE(SUM(p.marks_available)::int, 0) AS available
    FROM progress p
    WHERE p.course_id = c.id
      AND p.learner_id = @learner_id
      AND p.entity = 'assessment'
) mark_counts ON true
WHERE c.institution_id = @institution_id
  AND c.published = true
ORDER BY c.title;