-- Data exports and privacy erasure. Every export query is tenant/user scoped;
-- ListPurgeableDataExports is the deliberate cross-tenant retention pass.

-- name: CreateDataExport :one
INSERT INTO data_exports (tenant_id, user_id)
VALUES (@tenant_id::uuid, @user_id::uuid)
RETURNING *;

-- name: GetDataExport :one
SELECT * FROM data_exports
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid;

-- name: ListDataExportsCursor :many
SELECT * FROM data_exports
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: MarkDataExportRunning :execrows
UPDATE data_exports
SET status = 'running', updated_at = NOW()
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND status IN ('queued', 'failed', 'running');

-- name: MarkDataExportReady :execrows
UPDATE data_exports
SET status = 'ready', storage_path = @storage_path::text,
    expires_at = @expires_at::timestamptz, updated_at = NOW()
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND status = 'running';

-- name: MarkDataExportFailed :exec
UPDATE data_exports
SET status = 'failed', updated_at = NOW()
WHERE id = @id::uuid
  AND tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND status IN ('queued', 'running', 'failed');

-- name: ExpireDataExportsForUser :exec
UPDATE data_exports
SET status = 'expired', expires_at = COALESCE(expires_at, NOW()), updated_at = NOW()
WHERE tenant_id = @tenant_id::uuid
  AND user_id = @user_id::uuid
  AND status <> 'expired';

-- name: DeleteDataExportsForUser :exec
DELETE FROM data_exports
WHERE tenant_id = @tenant_id::uuid AND user_id = @user_id::uuid;

-- name: ListPurgeableDataExports :many
SELECT * FROM data_exports
WHERE status = 'expired'
   OR (expires_at IS NOT NULL AND expires_at <= NOW())
   -- A crashed/timed-out export or a failed build must not stay active
   -- forever: the job timeout is 10 minutes, so a day is stale.
   OR (status IN ('queued', 'running', 'failed') AND updated_at <= NOW() - INTERVAL '1 day')
ORDER BY id
LIMIT @batch_size::int;

-- name: DeleteDataExport :exec
DELETE FROM data_exports WHERE id = @id::uuid;

-- name: ListUserSessionsForPrivacy :many
SELECT * FROM sessions
WHERE tenant_id = @tenant_id::uuid AND user_id = @user_id::uuid
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: DeleteTenantUserSessions :exec
DELETE FROM sessions
WHERE tenant_id = @tenant_id::uuid AND user_id = @user_id::uuid;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = @user_id::uuid;

-- name: AnonymizeUser :one
-- Also upgrades an account deleted by the older self-service path, which left
-- the email, name, and credentials behind. A fully erased row matches nothing.
UPDATE users
SET email = @email::text,
    name = '',
    email_verified_at = NULL,
    status = 'deleted',
    deleted_at = COALESCE(deleted_at, NOW()),
    token_version = token_version + 1,
    credentials_changed_at = NOW()
WHERE id = @id::uuid
  AND (status <> 'deleted' OR email <> @email::text OR name <> ''
       OR email_verified_at IS NOT NULL)
RETURNING *;

-- name: DeleteUserPasswords :exec
DELETE FROM user_passwords WHERE user_id = @user_id::uuid;

-- name: DeleteUserIdentities :exec
DELETE FROM user_identities WHERE user_id = @user_id::uuid;

-- name: AnonymizeErasedUserInvitations :execrows
-- Erasure leaves no address in invitations the user accepted or that were
-- addressed to them; accepted_by stays the membership id. The users subselect
-- also covers an account an older path already marked deleted. A pending row
-- is unique per (tenant, address), so replacing the address cannot collide.
UPDATE invitations
SET email_normalized = @replacement::text,
    updated_at = NOW()
WHERE tenant_id = @tenant_id::uuid
  AND (accepted_by = @user_id::uuid
       OR email_normalized = (SELECT email FROM users WHERE id = @user_id::uuid))
  AND email_normalized <> @replacement::text;

-- name: AnonymizeErasedUserInvitationsAllTenants :execrows
-- The global finalizer runs under the RLS bypass after every membership is
-- removed. It also anonymizes invitations addressed to the erased address in a
-- tenant the user never joined, so no tenant keeps the address after a global
-- erasure.
UPDATE invitations
SET email_normalized = @replacement::text,
    updated_at = NOW()
WHERE (accepted_by = @user_id::uuid
       OR email_normalized = (SELECT email FROM users WHERE id = @user_id::uuid))
  AND email_normalized <> @replacement::text;

-- name: ListTenantDataExportsForPurge :many
-- Tenant purge: one bounded page of the tenant's exports (including rows of
-- memberships already erased) so their blobs can be deleted before the rows.
SELECT id, COALESCE(storage_path, '') AS storage_path
FROM data_exports
WHERE tenant_id = @tenant_id::uuid
LIMIT @batch_size::int;

-- name: DeleteTenantDataExportsForPurge :execrows
-- Tenant purge: deletes exactly the page whose blobs the caller just removed.
DELETE FROM data_exports
WHERE tenant_id = @tenant_id::uuid
  AND id = ANY(@ids::uuid[]);

-- name: ListUserExternalIdentities :many
-- Export: linked external accounts, without any token material (none is
-- stored). The subject identifies the account at the provider.
SELECT provider, subject, created_at
FROM user_identities
WHERE user_id = $1
ORDER BY provider, created_at, subject
LIMIT @row_limit::int;

-- name: ListUserInvitationsSent :many
-- Export: invitations the user created in this tenant.
SELECT i.id, i.email_normalized, r.key AS role, i.invited_by, i.expires_at, i.accepted_at,
       i.accepted_by, i.created_at
FROM invitations i
JOIN roles r ON r.id = i.role_id
WHERE i.tenant_id = @tenant_id::uuid AND i.invited_by = @user_id::uuid
ORDER BY i.created_at DESC, i.id DESC
LIMIT @row_limit::int;

-- name: ListUserInvitationsReceived :many
-- Export: invitations addressed to the user's current address or accepted by
-- the user in this tenant.
SELECT i.id, i.email_normalized, r.key AS role, i.invited_by, i.expires_at, i.accepted_at,
       i.accepted_by, i.created_at
FROM invitations i
JOIN roles r ON r.id = i.role_id
WHERE i.tenant_id = @tenant_id::uuid
  AND (i.accepted_by = @user_id::uuid OR i.email_normalized = @email::text)
ORDER BY i.created_at DESC, i.id DESC
LIMIT @row_limit::int;
