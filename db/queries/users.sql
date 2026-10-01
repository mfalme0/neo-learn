-- Neo Learn read queries.
--
-- Every query is institution-scoped. There is no query in this file that
-- filters on institution_id from the caller, because forgetting to do so is
-- a cross-tenant data leak.

-- name: GetUserByEmail :one
SELECT
    id,
    institution_id,
    email,
    display_name,
    role,
    password_hash
FROM users
WHERE institution_id = $1
  AND email = $2;

-- name: GetInstitutionBySlug :one
SELECT id, slug, name
FROM institutions
WHERE slug = $1;

-- name: GetInstitutionByID :one
SELECT id, slug, name
FROM institutions
WHERE id = $1;

-- name: UpdatePasswordHash :exec
-- Used to transparently upgrade a hash when the argon2 cost parameters are
-- raised. Scoped by institution so a user id cannot be used to reach across
-- tenants.
UPDATE users
SET password_hash = $3,
    updated_at = now()
WHERE id = $2
  AND institution_id = $1;

-- name: GetUserByID :one
SELECT
    id,
    institution_id,
    email,
    display_name,
    role
FROM users
WHERE institution_id = $1
  AND id = $2;

-- name: InsertSession :one
INSERT INTO sessions (institution_id, user_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: RevokeSession :exec
UPDATE sessions
SET revoked_at = now()
WHERE token_hash = $1
  AND revoked_at IS NULL;

-- Records activity for audit. Best-effort: a failure to update this row must
-- never fail a login.
-- name: TouchSession :exec
UPDATE sessions
SET last_seen_at = now()
WHERE token_hash = $1
  AND revoked_at IS NULL
  AND expires_at > now();