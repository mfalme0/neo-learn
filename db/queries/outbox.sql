-- Outbox drain queries for the SMS worker.
--
-- The worker is the first reader of the table written in milestone 1. It
-- follows the transactional outbox pattern: rows are claimed with a short lock,
-- attempted, then either marked delivered or released with a backoff.

-- name: ClaimDueOutboxRows :many
-- Takes a batch of messages that are due for a delivery attempt.
--
-- FOR UPDATE SKIP LOCKED is what makes multiple workers safe. Without it two
-- workers would read the same row and send the message twice -- and since SMS
-- is a real cost per message, that is not an acceptable failure mode.
--
-- A row is due when it is undelivered, not dead, and next_attempt_at has
-- passed. A stale claim is reclaimable after `stale_after`: a worker killed
-- mid-attempt must not strand its messages forever.
SELECT
    id,
    institution_id,
    event_id,
    topic,
    payload,
    destination_msisdn,
    created_at,
    attempts
FROM outbox
WHERE delivered_at IS NULL
  AND dead_at IS NULL
  AND destination_msisdn IS NOT NULL
  AND next_attempt_at <= now()
  AND (claimed_at IS NULL OR claimed_at < now() - make_interval(secs => sqlc.arg('stale_after_seconds')::int))
ORDER BY next_attempt_at, id
LIMIT sqlc.arg('batch_size')::int
FOR UPDATE SKIP LOCKED;

-- name: ClaimOutboxRow :one
-- Marks a single row claimed. Separate from ClaimDueOutboxRows so the lock is
-- taken only after the batch is known, keeping the transaction short.
UPDATE outbox
SET claimed_at = now(),
    attempts = attempts + 1
WHERE id = $1
  AND delivered_at IS NULL
  AND dead_at IS NULL
RETURNING id, attempts;

-- name: MarkOutboxDelivered :exec
-- Records a successful send along with the provider's own id, so a late
-- delivery report can be matched back to this row.
--
-- The provider id is nullable because the Sender interface does not surface
-- one for every provider; when absent the row still records that it went out.
UPDATE outbox
SET delivered_at = now(),
    provider_message_id = $2,
    last_error = NULL
WHERE id = $1
  AND delivered_at IS NULL;

-- name: ReleaseOutboxRow :exec
-- Returns a failed message to the queue with a backoff.
--
-- `next_attempt_at` is computed by the caller and passed in, so the backoff
-- policy lives in Go where it is testable rather than being scattered across
-- SQL.
--
-- dead_at is passed as a nullable timestamptz rather than a boolean + now(), so
-- codegen produces a plain pgtype.Timestamptz the caller can set to NULL
-- explicitly. An unset dead_at is indistinguishable from "not dead", which is
-- what makes the partial index on dead_at work.
--
-- claimed_at is cleared so another worker can pick the row up immediately.
UPDATE outbox
SET claimed_at = NULL,
    last_error = $2,
    next_attempt_at = $3,
    dead_at = $4
WHERE id = $1
  AND delivered_at IS NULL;

-- name: GetOutboxRow :one
SELECT
    id,
    topic,
    payload,
    destination_msisdn,
    attempts,
    delivered_at,
    dead_at,
    last_error
FROM outbox
WHERE id = $1;

-- name: GetOutboxByProviderMessageID :one
-- Matches an inbound delivery report to the row that produced it. Some
-- gateways report delivery long after the send, so this lookup may be the only
-- way to tie the two together.
SELECT id, delivered_at IS NOT NULL AS already_delivered
FROM outbox
WHERE provider_message_id = $1;

-- name: CountUndeliveredOutbox :one
-- Used by the health endpoint to surface a backed-up queue.
SELECT
    count(*) FILTER (WHERE delivered_at IS NULL AND dead_at IS NULL) AS pending,
    count(*) FILTER (WHERE dead_at IS NOT NULL)                      AS dead
FROM outbox;

-- name: RequeueDeadOutbox :execrows
-- Operator action: put dead messages back in the queue after investigating.
-- Not exposed over HTTP yet; it is destructive and deserves a deliberate UI.
UPDATE outbox
SET dead_at = NULL,
    claimed_at = NULL,
    next_attempt_at = now(),
    attempts = 0
WHERE id = $1 AND dead_at IS NOT NULL;