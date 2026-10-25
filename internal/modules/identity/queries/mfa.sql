-- name: GetMFAFactor :one
SELECT * FROM mfa_factors
WHERE user_id = @user_id AND type = 'totp';

-- name: GetConfirmedMFAFactor :one
SELECT * FROM mfa_factors
WHERE user_id = @user_id AND type = 'totp' AND confirmed_at IS NOT NULL;

-- name: UpsertUnconfirmedMFAFactor :one
-- One pending enrollment per user. A concurrent re-enrollment keeps the secret
-- the first writer stored (the no-op update), so every caller can return the
-- same still-valid provisioning URI.
INSERT INTO mfa_factors (user_id, type, secret_ciphertext)
VALUES (@user_id, 'totp', @secret_ciphertext)
ON CONFLICT (user_id) WHERE type = 'totp' AND confirmed_at IS NULL
DO UPDATE SET secret_ciphertext = mfa_factors.secret_ciphertext
RETURNING *;

-- name: ConfirmMFAFactor :execrows
UPDATE mfa_factors
SET confirmed_at = NOW(), last_used_step = @step
WHERE id = @id AND user_id = @user_id AND confirmed_at IS NULL;

-- name: ConsumeMFAStep :execrows
-- Replay guard: the verified step must be newer than every step this factor
-- has already accepted. Concurrent uses of the same code update one row total.
UPDATE mfa_factors
SET last_used_step = @step
WHERE user_id = @user_id AND type = 'totp' AND confirmed_at IS NOT NULL
  AND last_used_step < @step;

-- name: DeleteMFAFactor :exec
DELETE FROM mfa_factors
WHERE user_id = @user_id AND type = 'totp';

-- name: DeleteRecoveryCodes :exec
DELETE FROM mfa_recovery_codes
WHERE user_id = @user_id;

-- name: CreateRecoveryCodes :exec
INSERT INTO mfa_recovery_codes (user_id, code_hash)
SELECT @user_id, unnest(@code_hashes::text[]);

-- name: UseRecoveryCode :execrows
-- Single use: only the first concurrent request can claim a code.
UPDATE mfa_recovery_codes
SET used_at = NOW()
WHERE user_id = @user_id AND code_hash = @code_hash AND used_at IS NULL;

-- name: CountUnusedRecoveryCodes :one
SELECT count(*) FROM mfa_recovery_codes
WHERE user_id = @user_id AND used_at IS NULL;
