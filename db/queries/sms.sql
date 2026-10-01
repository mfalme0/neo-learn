-- SMS conversation and inbound-routing queries.
--
-- Everything here exists to answer one question: a reply arrives carrying only
-- a phone number and some text, and we have to work out what it means.

-- name: GetInstitutionsForMSISDN :many
-- Every institution a number is registered at.
--
-- Returns a list rather than a single row on purpose. A human can legitimately
-- be a learner at two schools, and silently picking one would let a reply be
-- applied to the wrong tenant's records. The inbound path treats more than one
-- row as unresolvable.
SELECT DISTINCT institution_id
FROM users
WHERE msisdn = $1
  AND password_hash IS NOT NULL
ORDER BY institution_id;

-- name: GetCorrectChoiceLabel :one
-- The correct answer for a question.
--
-- Read only to explain an incorrect reply. This is never projected into a
-- question prompt: doing so would put the answer key in the message the learner
-- sees before answering.
SELECT label
FROM choices
WHERE question_id = $1
  AND institution_id = $2
  AND is_correct
LIMIT 1;

-- name: GetUserByMSISDN :one
-- Inbound routing: turns the sender number into a learner.
--
-- Scoped by institution because the same human can be a student at two
-- schools, and an answer must be applied to the right one. The caller learns
-- the institution from the sender id the gateway was configured with, not from
-- anything the message body contains.
SELECT
    id,
    institution_id,
    display_name,
    msisdn
FROM users
WHERE msisdn = $1
  AND institution_id = $2
  AND password_hash IS NOT NULL
LIMIT 1;

-- name: UpsertSmsConversation :one
-- Creates or updates the conversation's pending target.
--
-- ON CONFLICT covers the (institution_id, learner_id) unique index, so one
-- conversation exists per learner per institution. pending_position and
-- pending_total are refreshed because an assessment may be re-sent after new
-- questions are added.
INSERT INTO sms_conversations (
    institution_id,
    learner_id,
    state,
    pending_assessment_id,
    pending_question_id,
    pending_lesson_id,
    pending_position,
    pending_total,
    opted_out,
    last_message_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
ON CONFLICT (institution_id, learner_id) DO UPDATE SET
    state = EXCLUDED.state,
    pending_assessment_id = EXCLUDED.pending_assessment_id,
    pending_question_id = EXCLUDED.pending_question_id,
    pending_lesson_id = EXCLUDED.pending_lesson_id,
    pending_position = EXCLUDED.pending_position,
    pending_total = EXCLUDED.pending_total,
    opted_out = EXCLUDED.opted_out,
    last_message_at = now(),
    updated_at = now()
RETURNING id, state, pending_assessment_id, pending_question_id,
          pending_lesson_id, pending_position, pending_total, opted_out;

-- name: GetSmsConversation :one
SELECT
    id,
    institution_id,
    learner_id,
    state,
    pending_assessment_id,
    pending_question_id,
    pending_lesson_id,
    pending_position,
    pending_total,
    opted_out,
    last_message_at
FROM sms_conversations
WHERE institution_id = $1
  AND learner_id = $2;

-- name: CloseSmsConversation :exec
-- Returns a conversation to idle, clearing the pending target.
--
-- The CHECK constraint requires the pending columns to be NULL in the idle and
-- completed states, so this cannot be done by updating state alone.
UPDATE sms_conversations
SET state = $3,
    pending_assessment_id = NULL,
    pending_question_id = NULL,
    pending_lesson_id = NULL,
    pending_position = 0,
    pending_total = 0,
    updated_at = now()
WHERE institution_id = $1
  AND learner_id = $2;

-- name: SetSmsOptOut :exec
-- Honours a STOP immediately. The inbound path checks this before sending
-- anything, so opt-out takes effect on the next message rather than the next
-- enrolment.
UPDATE sms_conversations
SET opted_out = $3,
    state = CASE WHEN $3 THEN 'idle'::sms_conversation_state ELSE state END,
    pending_assessment_id = CASE WHEN $3 THEN NULL ELSE pending_assessment_id END,
    pending_question_id = CASE WHEN $3 THEN NULL ELSE pending_question_id END,
    pending_lesson_id = CASE WHEN $3 THEN NULL ELSE pending_lesson_id END,
    updated_at = now()
WHERE institution_id = $1
  AND learner_id = $2;

-- name: CountEnrolledWithMSISDN :one
-- Used by the delivery scheduler to decide whether an SMS push is worth
-- attempting for an enrolled learner.
SELECT count(*)
FROM course_enrollments ce
JOIN users u ON u.id = ce.learner_id
WHERE ce.institution_id = $1
  AND u.msisdn IS NOT NULL
  AND u.role = 'student';

-- name: GetNextUnansweredQuestion :one
-- Finds the question after `after_position` that the learner has not answered
-- correctly yet.
--
-- Correctly, not merely answered: a learner who answered wrong should see the
-- question again with feedback rather than having it marked done. `limit` keeps
-- the query cheap regardless of assessment size.
SELECT q.id, q.prompt, q.marks, q.position
FROM questions q
WHERE q.assessment_id = $1
  AND q.institution_id = $2
  AND q.position > $3
  AND NOT EXISTS (
      SELECT 1
      FROM learning_events ev
      JOIN choices ch
        ON ch.question_id = ev.question_id
       AND ch.label = ev.payload ->> 'choice_label'
      WHERE ev.learner_id = $4
        AND ev.question_id = q.id
        AND ev.kind = 'question_answered'
        AND ch.is_correct
  )
ORDER BY q.position
LIMIT sqlc.arg('limit')::int;

-- name: CountCorrectAnswers :one
-- Used to decide whether an assessment is finished: every question answered
-- correctly, not merely attempted.
SELECT
    count(q.id) AS total,
    count(q.id) FILTER (WHERE answered.question_id IS NOT NULL) AS correct
FROM questions q
LEFT JOIN LATERAL (
    SELECT DISTINCT ON (ev.question_id) ev.question_id
    FROM learning_events ev
    JOIN choices ch
      ON ch.question_id = ev.question_id
     AND ch.label = ev.payload ->> 'choice_label'
    WHERE ev.learner_id = sqlc.arg('learner_id')::bigint
      AND ev.question_id = q.id
      AND ev.kind = 'question_answered'
      AND ch.is_correct
    ORDER BY ev.question_id, ev.occurred_at DESC, ev.id DESC
) answered ON true
WHERE q.assessment_id = $1;

-- name: GetAssessmentForSms :one
SELECT
    a.id,
    a.title,
    a.description,
    a.sms_eligible,
    a.course_id,
    a.institution_id
FROM assessments a
WHERE a.id = $1
  AND a.institution_id = $2
  AND a.published = true;

-- name: GetQuestionForSms :one
-- Fetches everything needed to render one question as text, including whether
-- the learner already got it right.
SELECT
    q.id,
    q.assessment_id,
    q.prompt,
    q.marks,
    q.position,
    a.title AS assessment_title,
    a.institution_id
FROM questions q
JOIN assessments a ON a.id = q.assessment_id
WHERE q.id = $1
  AND q.institution_id = $2;

-- name: ListChoicesForSms :many
-- Returns every choice with its label and body. Whether one is correct is
-- resolved server-side by the learning engine, never here: the rendering layer
-- must not be able to leak the answer key into a message.
SELECT label, body
FROM choices
WHERE question_id = $1
  AND institution_id = $2
ORDER BY position;

-- name: GetLessonForSms :one
SELECT
    l.id,
    l.title,
    l.body_markdown,
    l.sms_eligible,
    m.course_id,
    l.institution_id
FROM lessons l
JOIN modules m ON m.id = l.module_id
WHERE l.id = $1
  AND l.institution_id = $2;

-- name: ListAssessmentsForSms :many
-- Assessments a learner can be messaged about. Used by the delivery scheduler
-- to decide what to send next.
SELECT
    a.id,
    a.title,
    a.position
FROM assessments a
WHERE a.course_id = $1
  AND a.institution_id = $2
  AND a.published = true
  AND a.sms_eligible = true
ORDER BY a.position;

-- name: GetFirstUnansweredLesson :one
SELECT l.id, l.title, m.course_id
FROM lessons l
JOIN modules m ON m.id = l.module_id
WHERE m.course_id = $1
  AND l.institution_id = $2
  AND NOT EXISTS (
      SELECT 1
      FROM progress p
      WHERE p.learner_id = $3
        AND p.lesson_id = l.id
        AND p.status = 'completed'
  )
ORDER BY m.position, l.position
LIMIT 1;