-- Neo Learn initial schema.
--
-- Design notes that the rest of the codebase depends on:
--
--   * Every tenant-scoped table carries institution_id. There is no
--     single-tenant shortcut to unwind later.
--   * learning_events is the source of truth for learner state. It is
--     append-only. `progress` is a projection of that log, disposable and
--     rebuildable from it.
--   * event_id is a client-generated UUID with a UNIQUE constraint. That
--     single constraint is what makes duplicate SMS replies idempotent.
--   * occurred_at is the learner's clock, received_at is ours. The gap is
--     offline lag and should stay measurable.

-- citext makes email comparison case-insensitive without requiring the
-- application to remember to lower() on every lookup.
CREATE EXTENSION IF NOT EXISTS citext;

-- ---------------------------------------------------------------------------
-- Tenancy and identity
-- ---------------------------------------------------------------------------

CREATE TABLE institutions (
    id          BIGSERIAL PRIMARY KEY,
    slug        TEXT        NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE user_role AS ENUM ('admin', 'teacher', 'student');

CREATE TABLE users (
    id              BIGSERIAL PRIMARY KEY,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    email           CITEXT      NOT NULL,
    display_name    TEXT        NOT NULL,
    role            user_role   NOT NULL DEFAULT 'student',
    -- Encoded argon2id hash. Nullable so an SSO-only user can exist, but
    -- password login is then impossible for that account.
    password_hash   TEXT,
    -- E.164 normalized, e.g. +254700000001. Used to route SMS replies back
    -- to a learner. Unique per institution, not globally: the same human
    -- can be a student at two schools.
    msisdn          TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT users_email_not_blank CHECK (length(btrim(email)) > 0),
    CONSTRAINT users_msisdn_format CHECK (
        msisdn IS NULL OR msisdn ~ '^\+[1-9][0-9]{7,14}$'
    )
);

-- citext already compares case-insensitively, so a plain unique index is
-- enough; lower() would be redundant here.
CREATE UNIQUE INDEX users_institution_email_key
    ON users (institution_id, email);
CREATE UNIQUE INDEX users_institution_msisdn_key
    ON users (institution_id, msisdn)
    WHERE msisdn IS NOT NULL;

-- Sessions live in Redis. This table records issuance for audit and for
-- revoking every session belonging to a user at once.
CREATE TABLE sessions (
    id              BIGSERIAL PRIMARY KEY,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    user_id         BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- SHA-256 of the session token. The raw token is never stored.
    token_hash      TEXT        NOT NULL UNIQUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    revoked_at      TIMESTAMPTZ,

    CONSTRAINT sessions_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX sessions_user_active_idx
    ON sessions (user_id, expires_at)
    WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Course content
-- ---------------------------------------------------------------------------

CREATE TABLE courses (
    id              BIGSERIAL PRIMARY KEY,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    code            TEXT        NOT NULL,
    title           TEXT        NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    published       BOOLEAN     NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT courses_code_not_blank CHECK (length(btrim(code)) > 0),
    CONSTRAINT courses_title_not_blank CHECK (length(btrim(title)) > 0)
);

-- Codes are human-facing ("MATH101"), so uniqueness is per institution.
CREATE UNIQUE INDEX courses_institution_code_key
    ON courses (institution_id, code);

CREATE TABLE modules (
    id          BIGSERIAL PRIMARY KEY,
    course_id   BIGINT      NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    -- Denormalised so a module can be authorised without joining courses.
    institution_id BIGINT   NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    title       TEXT        NOT NULL,
    position    INT         NOT NULL,

    CONSTRAINT modules_title_not_blank CHECK (length(btrim(title)) > 0),
    CONSTRAINT modules_position_non_negative CHECK (position >= 0),
    CONSTRAINT modules_course_position_key UNIQUE (course_id, position)
);

CREATE TABLE lessons (
    id              BIGSERIAL PRIMARY KEY,
    module_id       BIGINT      NOT NULL REFERENCES modules (id) ON DELETE CASCADE,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    title           TEXT        NOT NULL,
    -- Body is markdown. Rendered client-side; the API stores source only.
    body_markdown    TEXT        NOT NULL DEFAULT '',
    -- Set once the lesson has been pushed over SMS. Content that cannot be
    -- split into SMS-sized chunks must not be flagged deliverable.
    sms_eligible     BOOLEAN     NOT NULL DEFAULT false,
    position        INT         NOT NULL,

    CONSTRAINT lessons_title_not_blank CHECK (length(btrim(title)) > 0),
    CONSTRAINT lessons_position_non_negative CHECK (position >= 0),
    CONSTRAINT lessons_module_position_key UNIQUE (module_id, position)
);

-- ---------------------------------------------------------------------------
-- Assessment
-- ---------------------------------------------------------------------------

CREATE TABLE assessments (
    id              BIGSERIAL PRIMARY KEY,
    course_id       BIGINT      NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    title           TEXT        NOT NULL,
    description     TEXT        NOT NULL DEFAULT '',
    published       BOOLEAN     NOT NULL DEFAULT false,
    -- Whether answers may arrive by SMS. Conceptually free-form assessment
    -- cannot be answered by a single-character reply.
    sms_eligible    BOOLEAN     NOT NULL DEFAULT false,
    position        INT         NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT assessments_title_not_blank CHECK (length(btrim(title)) > 0),
    CONSTRAINT assessments_position_non_negative CHECK (position >= 0),
    CONSTRAINT assessments_course_position_key UNIQUE (course_id, position)
);

CREATE TYPE question_kind AS ENUM ('single_choice');

CREATE TABLE questions (
    id              BIGSERIAL PRIMARY KEY,
    assessment_id   BIGINT      NOT NULL REFERENCES assessments (id) ON DELETE CASCADE,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    kind            question_kind NOT NULL DEFAULT 'single_choice',
    prompt          TEXT        NOT NULL,
    -- Marks awarded for a correct answer. Integer to keep the SMS and web
    -- paths producing identical sums.
    marks           INT         NOT NULL DEFAULT 1,
    position        INT         NOT NULL,

    CONSTRAINT questions_prompt_not_blank CHECK (length(btrim(prompt)) > 0),
    CONSTRAINT questions_marks_positive CHECK (marks > 0),
    CONSTRAINT questions_position_non_negative CHECK (position >= 0),
    CONSTRAINT questions_assessment_position_key UNIQUE (assessment_id, position)
);

-- Choices use single-character labels so an SMS reply can address one:
-- a learner texts "B", not a UUID.
CREATE TABLE choices (
    id              BIGSERIAL PRIMARY KEY,
    question_id     BIGINT      NOT NULL REFERENCES questions (id) ON DELETE CASCADE,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    -- Single uppercase ASCII letter. Deliberately not a word: it has to
    -- survive being the entire contents of a text message.
    label           CHAR(1)     NOT NULL,
    body            TEXT        NOT NULL,
    is_correct      BOOLEAN     NOT NULL DEFAULT false,
    position        INT         NOT NULL,

    CONSTRAINT choices_label_uppercase CHECK (label ~ '^[A-Z]$'),
    CONSTRAINT choices_position_non_negative CHECK (position >= 0),
    CONSTRAINT choices_question_label_key UNIQUE (question_id, label),
    CONSTRAINT choices_question_position_key UNIQUE (question_id, position)
);

-- A single-choice question has exactly one correct answer. Enforced in the
-- application transaction that writes choices; a partial unique index cannot
-- express "at most one" without also forbidding zero during authoring.
COMMENT ON TABLE choices IS
    'Exactly one choice per question must have is_correct = true; enforced by the authoring service.';

-- ---------------------------------------------------------------------------
-- Enrolment
-- ---------------------------------------------------------------------------

CREATE TABLE course_enrollments (
    id              BIGSERIAL PRIMARY KEY,
    course_id       BIGINT      NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    -- The learner. A teacher may also be enrolled, so this is not constrained
    -- to role = 'student'.
    learner_id      BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    assigned_by     BIGINT      REFERENCES users (id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT course_enrollments_unique UNIQUE (course_id, learner_id)
);

CREATE INDEX course_enrollments_learner_idx
    ON course_enrollments (learner_id, institution_id);

-- ---------------------------------------------------------------------------
-- The event log: source of truth for learner state
-- ---------------------------------------------------------------------------

CREATE TYPE event_source AS ENUM ('web', 'sms', 'stk');

CREATE TYPE event_kind AS ENUM (
    'lesson_started',
    'lesson_completed',
    'question_answered',
    'assessment_submitted'
);

CREATE TABLE learning_events (
    id              BIGSERIAL PRIMARY KEY,
    -- Client-generated UUID. This is the idempotency key: an SMS gateway
    -- retry or an offline queue replay produces the same value, and the
    -- UNIQUE constraint below turns the second attempt into a no-op.
    event_id        UUID        NOT NULL,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    learner_id      BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    course_id       BIGINT      REFERENCES courses (id) ON DELETE CASCADE,
    lesson_id       BIGINT      REFERENCES lessons (id) ON DELETE CASCADE,
    assessment_id   BIGINT      REFERENCES assessments (id) ON DELETE CASCADE,
    question_id     BIGINT      REFERENCES questions (id) ON DELETE CASCADE,

    kind            event_kind  NOT NULL,
    source          event_source NOT NULL DEFAULT 'web',

    -- Kind-specific data, e.g. {"choice_label":"B","correct":true,"marks":1}.
    payload         JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- The learner's clock, and ours. Their difference is offline lag.
    occurred_at     TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT learning_events_event_id_key UNIQUE (event_id),
    -- 'lesson_completed' must reference a lesson; 'question_answered' must
    -- reference a question. Keeps the log self-describing so the SMS worker
    -- in milestone 2 can route on kind without a lookup table.
    CONSTRAINT learning_events_target_present CHECK (
        (kind = 'lesson_completed' AND lesson_id IS NOT NULL)
        OR (kind = 'lesson_started' AND lesson_id IS NOT NULL)
        OR (kind = 'question_answered' AND question_id IS NOT NULL)
        OR (kind = 'assessment_submitted' AND assessment_id IS NOT NULL)
    )
);

-- Replay order for a learner's log.
CREATE INDEX learning_events_learner_time_idx
    ON learning_events (learner_id, occurred_at, id);
-- Drain order for the outbox worker that will consume this log in M2.
CREATE INDEX learning_events_kind_time_idx
    ON learning_events (kind, received_at);

-- ---------------------------------------------------------------------------
-- Projection of the event log. Rebuildable; never authoritative.
-- ---------------------------------------------------------------------------

CREATE TABLE progress (
    id              BIGSERIAL PRIMARY KEY,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    learner_id      BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    course_id       BIGINT      NOT NULL REFERENCES courses (id) ON DELETE CASCADE,
    -- Exactly one of these is set, matching the 'entity' column.
    lesson_id       BIGINT      REFERENCES lessons (id) ON DELETE CASCADE,
    assessment_id   BIGINT      REFERENCES assessments (id) ON DELETE CASCADE,
    entity          TEXT        NOT NULL,

    status          TEXT        NOT NULL DEFAULT 'not_started',
    -- Running totals, so a dashboard read is a single row lookup.
    questions_answered  INT     NOT NULL DEFAULT 0,
    questions_correct   INT     NOT NULL DEFAULT 0,
    marks_awarded       INT     NOT NULL DEFAULT 0,
    marks_available     INT     NOT NULL DEFAULT 0,
    -- 0-100, integer to avoid float drift between transport paths.
    percent          INT         NOT NULL DEFAULT 0,

    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT progress_entity_valid CHECK (entity IN ('lesson', 'assessment')),
    -- Exactly one target, and it matches the entity discriminator. A
    -- table-level UNIQUE cannot reference COALESCE, so the uniqueness rule
    -- lives in progress_learner_target_key below.
    CONSTRAINT progress_target_matches_entity CHECK (
        (entity = 'lesson' AND lesson_id IS NOT NULL AND assessment_id IS NULL)
        OR (entity = 'assessment' AND assessment_id IS NOT NULL AND lesson_id IS NULL)
    ),
    CONSTRAINT progress_status_valid CHECK (
        status IN ('not_started', 'in_progress', 'completed')
    ),
    CONSTRAINT progress_percent_range CHECK (percent BETWEEN 0 AND 100),
    CONSTRAINT progress_counts_non_negative CHECK (
        questions_answered >= 0
        AND questions_correct >= 0
        AND marks_awarded >= 0
        AND marks_available >= 0
    ),
    CONSTRAINT progress_correct_not_exceeding_answered CHECK (
        questions_correct <= questions_answered
    ),
    CONSTRAINT progress_completion_implies_completed_at CHECK (
        status <> 'completed' OR completed_at IS NOT NULL
    )
);

-- One row per learner per target. NULL != NULL under a plain unique index,
-- so the nullable columns are coalesced to 0 to make the constraint hold.
CREATE UNIQUE INDEX progress_learner_target_key
    ON progress (learner_id, entity, COALESCE(lesson_id, 0), COALESCE(assessment_id, 0));

CREATE INDEX progress_learner_course_idx
    ON progress (learner_id, course_id);

-- ---------------------------------------------------------------------------
-- Transactional outbox
--
-- Rows here are written in the same transaction as the event, so a state
-- change and its notification cannot diverge. Nothing reads this table until
-- milestone 2 (the SMS worker); it exists now so the ingest path does not
-- have to be rewritten when that lands.
-- ---------------------------------------------------------------------------

CREATE TABLE outbox (
    id              BIGSERIAL PRIMARY KEY,
    institution_id  BIGINT      NOT NULL REFERENCES institutions (id) ON DELETE CASCADE,
    -- The event that caused this row. Lets the worker skip messages whose
    -- originating event was rolled back or deduplicated.
    event_id        UUID        NOT NULL REFERENCES learning_events (event_id) ON DELETE CASCADE,
    topic           TEXT        NOT NULL,
    payload         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    -- Where this message should go. NULL for events with no outbound message.
    destination_msisdn TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Claimed by a worker. NULL until then. Separate from created_at so a
    -- stuck message is visible without parsing timestamps.
    claimed_at      TIMESTAMPTZ,
    delivered_at    TIMESTAMPTZ,
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT,

    CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0),
    -- Delivery state is derived from these two, never stored separately.
    CONSTRAINT outbox_delivery_state_valid CHECK (
        (delivered_at IS NOT NULL AND claimed_at IS NOT NULL)
        OR (delivered_at IS NULL)
    )
);

CREATE INDEX outbox_undelivered_idx
    ON outbox (created_at)
    WHERE delivered_at IS NULL;