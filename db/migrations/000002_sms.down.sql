-- Reverse of 000002_sms.

DROP TABLE IF EXISTS sms_conversations;

DROP TYPE IF EXISTS sms_conversation_state;

DROP INDEX IF EXISTS users_msisdn_lookup_idx;

-- The partial indexes depend on the columns being dropped, so they go first.
DROP INDEX IF EXISTS outbox_provider_message_key;
DROP INDEX IF EXISTS outbox_dead_idx;
DROP INDEX IF EXISTS outbox_due_idx;

ALTER TABLE outbox
    DROP COLUMN IF EXISTS provider_message_id,
    DROP COLUMN IF EXISTS dead_at,
    DROP COLUMN IF EXISTS next_attempt_at;