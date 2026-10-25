-- name: CreateRefreshToken :exec
INSERT INTO refresh_tokens (tenant_id, session_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4);

-- name: GetRefreshTokenByHash :one
-- Possession-based lookup (the hash is unique across tenants); the service
-- compares the row's tenant with the request tenant before using it.
SELECT * FROM refresh_tokens
WHERE token_hash = $1;

-- name: MarkRefreshTokenUsed :execrows
-- Exactly one concurrent rotation can win; the loser sees zero rows and is
-- treated as reuse.
UPDATE refresh_tokens
SET used_at = NOW()
WHERE tenant_id = $1 AND id = $2 AND used_at IS NULL;

-- name: PurgeExpiredRefreshTokens :execrows
-- Maintenance purge across every tenant (an expired token is dead in any
-- tenant). One bounded batch per call; SKIP LOCKED avoids waiting on rows a
-- concurrent purge or rotation already holds. Served by
-- idx_refresh_tokens_expires_at.
WITH batch AS (
    SELECT id FROM refresh_tokens
    WHERE expires_at < now()
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM refresh_tokens AS t
USING batch
WHERE t.id = batch.id;
