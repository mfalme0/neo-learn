-- Milestone 2: SMS transport support.
--
-- Adds two things:
--
--   1. Retry scheduling on the outbox. Milestone 1 wrote rows but had no
--      reader, so `claimed_at` and `attempts` went unused. A drain worker needs
--      to know when a message is *due* again, not merely that it failed.
--
--   2. sms_conversations, the state that tells an inbound reply which question
--      it is answering.
--
-- The second table is transport state, not learning state. Learning state lives
-- in learning_events and must not be duplicated here. What a conversation holds
-- is "what did we last ask this learner, and what may they reply about" --
-- which is genuinely unknowable from the event log alone, because the log
-- records what happened, not what was asked.

ALTER TABLE outbox
    -- When this row next becomes eligible for a delivery attempt. Set to now()
    -- on insert; the worker pushes it forward on failure using exponential
    -- backoff. Distinct from created_at (when the message was authored) and
    -- claimed_at (when a worker last picked it up).
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Partial index over undelivered rows only. The worker's hot query is always
-- "what is due now", and this stays small because delivered rows leave it.
CREATE INDEX outbox_due_idx
    ON outbox (next_attempt_at, id)
    WHERE delivered_at IS NULL;

-- A message that has failed repeatedly is a problem a human should see rather
-- than something to retry forever. Status is derived from attempts and
-- next_attempt_at; it is not stored, to avoid a second source of truth.
COMMENT ON COLUMN outbox.attempts IS
    'Number of delivery attempts. At or above the give-up threshold the row stops being retried and dead_at is set.';

ALTER TABLE outbox
    ADD COLUMN dead_at TIMESTAMPTZ;

CREATE INDEX outbox_dead_idx
    ON outbox (dead_at)
    WHERE dead_at IS NOT NULL;

-- A delivery report can arrive out of order relative to our own send, and some
-- gateways report delivery long after the fact. Recording the provider's own
-- message id lets a late report be matched to the row it belongs to.
ALTER TABLE outbox
    ADD COLUMN provider_message_id TEXT;

CREATE UNIQUE INDEX outbox_provider_message_key
    ON outbox (provider_message_id)
    WHERE provider_message_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- SMS conversations
-- ---------------------------------------------------------------------------

CREATE TYPE sms_conversation_state AS ENUM (
    'idle',
    'awaiting_answer',
    'awaiting_start',
    'completed'
);

CREATE TABLE sms_conversations (
    id              BIGSERIAL PRIMARY KEY,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    learner_id      BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,

    -- One conversation per learner per institution. A learner enrolled at two
    -- schools has two, and an answer from one must never advance the other.
    UNIQUE (institution_id, learner_id),

    state           sms_conversation_state NOT NULL DEFAULT 'idle',

    -- What the learner was last asked about. A reply is only meaningful
    -- against the thing that was actually sent, so these are cleared when the
    -- conversation closes rather than left to rot.
    pending_assessment_id BIGINT REFERENCES assessments (id) ON DELETE CASCADE,
    pending_question_id   BIGINT REFERENCES questions (id) ON DELETE CASCADE,
    pending_lesson_id     BIGINT REFERENCES lessons (id) ON DELETE CASCADE,

    -- Position within the assessment, 1-based, for the "Question 3/10" line.
    pending_position  INT         NOT NULL DEFAULT 0,
    pending_total     INT         NOT NULL DEFAULT 0,

    -- Opt-out. A learner replying STOP must stop receiving messages, and the
    -- inbound path has to be able to honour that on the very next message
    -- rather than only on the next enrolment.
    opted_out         BOOLEAN     NOT NULL DEFAULT false,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_message_at  TIMESTAMPTZ,

    CONSTRAINT sms_conversations_target_matches_state CHECK (
        (state = 'awaiting_answer'
             AND pending_question_id IS NOT NULL
             AND pending_assessment_id IS NOT NULL)
        OR (state = 'awaiting_start' AND pending_lesson_id IS NOT NULL)
        OR (state IN ('idle', 'completed')
             AND pending_question_id IS NULL
             AND pending_assessment_id IS NULL
             AND pending_lesson_id IS NULL)
    ),
    CONSTRAINT sms_conversations_position_non_negative CHECK (
        pending_position >= 0 AND pending_total >= 0
    )
);

CREATE INDEX sms_conversations_awaiting_idx
    ON sms_conversations (institution_id, updated_at)
    WHERE state IN ('awaiting_answer', 'awaiting_start');

-- Inbound routing: a reply arrives with only a sender number, so this is the
-- lookup that turns a phone number into a learner.
CREATE INDEX users_msisdn_lookup_idx
    ON users (msisdn)
    WHERE msisdn IS NOT NULL;

COMMENT ON TABLE sms_conversations IS
    'Transport state for SMS. Holds what was asked, not what was answered -- answers live in learning_events.';