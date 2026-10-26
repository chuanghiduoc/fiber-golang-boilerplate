-- name: UpsertInvitation :one
-- One pending invitation per (tenant, address): re-inviting replaces the
-- token, role, inviter, and expiry of the pending row, so the newest emailed
-- link is the only valid one.
INSERT INTO invitations (tenant_id, email_normalized, role, token_hash, invited_by, expires_at)
VALUES (@tenant_id, @email_normalized, @role, @token_hash, @invited_by, @expires_at)
ON CONFLICT (tenant_id, email_normalized) WHERE accepted_at IS NULL DO UPDATE
SET role = EXCLUDED.role,
    token_hash = EXCLUDED.token_hash,
    invited_by = EXCLUDED.invited_by,
    expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: ListPendingInvitationsCursor :many
SELECT * FROM invitations
WHERE tenant_id = @tenant_id::bigint
  AND accepted_at IS NULL
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: DeletePendingInvitation :execrows
DELETE FROM invitations
WHERE tenant_id = $1 AND id = $2 AND accepted_at IS NULL;

-- name: GetInvitationByTokenHashForUpdate :one
-- Possession-based lookup: the invitee may not be a member of (or signed in
-- to) the inviting tenant yet. The row lock serializes concurrent accepts.
SELECT * FROM invitations
WHERE token_hash = $1
FOR UPDATE;

-- name: MarkInvitationAccepted :execrows
UPDATE invitations
SET accepted_at = NOW(), accepted_by = @accepted_by
WHERE tenant_id = @tenant_id AND id = @id AND accepted_at IS NULL;

-- name: DeletePendingInvitationsByInviter :execrows
-- A ban or removal ends every pending invitation the member created: the
-- invitation no longer reflects a live grant of authority.
DELETE FROM invitations
WHERE tenant_id = @tenant_id AND invited_by = @invited_by AND accepted_at IS NULL;

-- name: PurgeExpiredInvitations :execrows
-- Maintenance purge across every tenant: expired pending invitations can
-- never be accepted. Accepted rows are kept as the membership's provenance.
WITH batch AS (
    SELECT id FROM invitations
    WHERE accepted_at IS NULL AND expires_at < now()
    LIMIT @batch_size::int
    FOR UPDATE SKIP LOCKED
)
DELETE FROM invitations AS t
USING batch
WHERE t.id = batch.id;

-- name: DeleteTenantInvitations :exec
-- Tenant purge: invitations reference memberships through invited_by and
-- accepted_by (RESTRICT), so they must go before the memberships.
DELETE FROM invitations WHERE tenant_id = $1;
