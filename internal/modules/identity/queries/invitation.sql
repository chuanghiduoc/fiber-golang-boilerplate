-- name: UpsertInvitation :one
-- One pending invitation per (tenant, address): re-inviting replaces the
-- token, role, inviter, and expiry of the pending row, so the newest emailed
-- link is the only valid one. role_id grants a tenant role on acceptance.
INSERT INTO invitations (tenant_id, email_normalized, role_id, token_hash, invited_by, expires_at)
VALUES (@tenant_id, @email_normalized, @role_id, @token_hash, @invited_by, @expires_at)
ON CONFLICT (tenant_id, email_normalized) WHERE accepted_at IS NULL DO UPDATE
SET role_id = EXCLUDED.role_id,
    token_hash = EXCLUDED.token_hash,
    invited_by = EXCLUDED.invited_by,
    expires_at = EXCLUDED.expires_at
RETURNING *;

-- name: ListPendingInvitationsCursor :many
SELECT sqlc.embed(i), r.key AS role
FROM invitations i
JOIN roles r ON r.id = i.role_id
WHERE i.tenant_id = @tenant_id::uuid
  AND i.accepted_at IS NULL
  AND (i.created_at, i.id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY i.created_at DESC, i.id DESC
LIMIT @row_limit::int;

-- name: GetInvitation :one
SELECT sqlc.embed(i), r.key AS role
FROM invitations i
JOIN roles r ON r.id = i.role_id
WHERE i.tenant_id = @tenant_id::uuid AND i.id = @id::uuid;

-- name: DeletePendingInvitation :execrows
DELETE FROM invitations
WHERE tenant_id = $1 AND id = $2 AND accepted_at IS NULL;

-- name: GetInvitationTenantByTokenHash :one
-- Possession-based pre-read: the invitee may not be signed in to (or a member
-- of) the inviting tenant yet, so the tenant is resolved by token hash alone
-- before the tenant-scoped transaction starts. The invitation is re-locked
-- and re-validated under that tenant's scope.
SELECT tenant_id FROM invitations
WHERE token_hash = $1;

-- name: GetInvitationByTokenHashForUpdate :one
-- Tenant-scoped possession lookup: the row lock serializes concurrent accepts.
-- tenant_id is explicit so the statement stays safe even under RLS bypass.
SELECT sqlc.embed(i), r.key AS role
FROM invitations i
JOIN roles r ON r.id = i.role_id
WHERE i.tenant_id = @tenant_id AND i.token_hash = @token_hash
FOR UPDATE OF i;

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

-- name: DeleteMemberInvitations :execrows
-- Tenant purge releases the RESTRICT foreign keys before deleting one
-- membership: invitations this member created or accepted. The tenant-wide
-- DeleteTenantInvitationsBatch removes the remaining rows later; the tenant is
-- already deleted, so its provenance has no reader left.
DELETE FROM invitations
WHERE tenant_id = @tenant_id::uuid
  AND (invited_by = @user_id::uuid OR accepted_by = @user_id::uuid);

-- name: DeleteTenantInvitationsBatch :execrows
-- Tenant purge: invitations reference memberships through invited_by and
-- accepted_by (RESTRICT), so they must go before the memberships. One bounded
-- page per statement keeps a large tenant from loading everything at once.
DELETE FROM invitations
WHERE id IN (
    SELECT id FROM invitations
    WHERE tenant_id = @tenant_id::uuid
    LIMIT @batch_size::int
);
