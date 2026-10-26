-- Only trialing and active tenants serve requests ("operational"). A trialing
-- tenant stops serving the moment trial_ends_at passes, before the hourly
-- expire-trials job changes its status.

-- name: GetOperationalTenantBySlug :one
SELECT * FROM tenants
WHERE slug = $1
  AND (status = 'active' OR (status = 'trialing' AND trial_ends_at > NOW()));

-- name: GetOperationalTenantByID :one
SELECT * FROM tenants
WHERE id = $1
  AND (status = 'active' OR (status = 'trialing' AND trial_ends_at > NOW()));

-- name: GetTenantByID :one
-- Startup checks need existence independent of request eligibility so a
-- suspended configured/default tenant does not crash the process.
SELECT * FROM tenants
WHERE id = $1;

-- name: GetTenantBySlug :one
SELECT * FROM tenants
WHERE slug = $1;

-- name: GetTenantByIDForUpdate :one
SELECT * FROM tenants
WHERE id = $1
FOR UPDATE;

-- name: ListTenantsCursor :many
-- Platform administration: every tenant, newest first.
SELECT * FROM tenants
WHERE (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY created_at DESC, id DESC
LIMIT @row_limit::int;

-- name: CreateTenant :one
INSERT INTO tenants (slug, name, status, trial_ends_at)
VALUES (@slug, @name, @status, @trial_ends_at)
RETURNING *;

-- name: CreateTenantSettings :exec
INSERT INTO tenant_settings (tenant_id, locale, timezone)
VALUES ($1, $2, $3);

-- name: UpdateTenantStatus :one
UPDATE tenants
SET status = @status,
    status_changed_at = NOW(),
    deletion_scheduled_at = @deletion_scheduled_at
WHERE id = @id
RETURNING *;

-- name: GetTenantSettings :one
SELECT * FROM tenant_settings
WHERE tenant_id = $1;

-- name: ListExpiredTrialTenantIDs :many
-- The hourly trial-expiry job moves these to suspended through the audited
-- tenant service. Bounded so one pass cannot scan an unbounded backlog.
SELECT id FROM tenants
WHERE status = 'trialing'
  AND trial_ends_at <= NOW()
ORDER BY id
LIMIT @row_limit::int;

-- name: ListDeletedUnpurgedTenants :many
-- The deleted-tenant purge only considers rows that have not completed yet.
SELECT * FROM tenants
WHERE status = 'deleted'
  AND purged_at IS NULL
ORDER BY id
LIMIT @row_limit::int;

-- name: MarkTenantPurged :execrows
-- Completion marker. The status check makes a concurrent status change a
-- no-op instead of marking an operational tenant purged.
UPDATE tenants
SET purged_at = NOW()
WHERE id = @id::bigint
  AND status = 'deleted'
  AND purged_at IS NULL;

-- name: DeleteTenantSettings :exec
-- Tenant purge: the deleted tenant's configuration has no reader left. The
-- tenants row itself is kept for audit and incident review.
DELETE FROM tenant_settings WHERE tenant_id = $1;
