-- name: CreatePasskey :one
INSERT INTO webauthn_credentials (
    user_id, credential_id, public_key, sign_count, aaguid, transports,
    backup_eligible, backup_state, name
)
VALUES (
    @user_id, @credential_id, @public_key, @sign_count, @aaguid, @transports,
    @backup_eligible, @backup_state, @name
)
RETURNING *;

-- name: ListPasskeysByUser :many
SELECT * FROM webauthn_credentials
WHERE user_id = @user_id
ORDER BY created_at DESC, id DESC;

-- name: ListPasskeysByUserLimited :many
-- Export: one bounded page of passkey metadata, newest first.
SELECT * FROM webauthn_credentials
WHERE user_id = @user_id
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: GetPasskeyByCredentialID :one
-- Assertions are global: the credential id is unique across users and is the
-- only value a discoverable login knows before resolving the owner.
SELECT * FROM webauthn_credentials
WHERE credential_id = @credential_id;

-- name: GetPasskey :one
SELECT * FROM webauthn_credentials
WHERE user_id = @user_id AND id = @id;

-- name: DeletePasskey :execrows
DELETE FROM webauthn_credentials
WHERE user_id = @user_id AND id = @id;

-- name: DeletePasskeysByUser :exec
DELETE FROM webauthn_credentials
WHERE user_id = @user_id;

-- name: UpdatePasskeyAfterAssertion :execrows
-- Conditional update: only one concurrent assertion can advance the counter.
-- A zero counter is valid for authenticators that do not implement one, so an
-- unchanged zero is accepted; any non-zero value must be strictly greater.
UPDATE webauthn_credentials
SET sign_count = @sign_count,
    backup_state = @backup_state,
    last_used_at = NOW(),
    updated_at = NOW()
WHERE id = @id AND user_id = @user_id
  AND (sign_count < @sign_count OR (@sign_count = 0 AND sign_count = 0));
