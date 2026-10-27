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
WHERE (created_at, id) < (@cursor_created_at::timestamptz, @cursor_id::uuid)
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

-- name: UpdateTenantName :one
UPDATE tenants
SET name = @name
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
-- The deleted-tenant purge only considers rows that have not completed yet,
-- least recently advanced first so a large tenant cannot starve the rest.
-- A NULL purge_progress_at (never attempted) comes first.
SELECT * FROM tenants
WHERE status = 'deleted'
  AND purged_at IS NULL
ORDER BY purge_progress_at ASC NULLS FIRST, id
LIMIT @row_limit::int;

-- name: AdvanceTenantPurgeCursor :exec
-- Records the highest membership user id whose erasers succeeded. It only
-- moves forward and only for an unfinished deleted tenant.
UPDATE tenants
SET purge_cursor = @purge_cursor::uuid,
    purge_progress_at = NOW()
WHERE id = @id::uuid
  AND status = 'deleted'
  AND purged_at IS NULL
  AND purge_cursor < @purge_cursor::uuid;

-- name: ListDueDeletionTenantIDs :many
-- The hourly finalize job moves tenants whose pending_deletion grace ended to
-- deleted through the audited TenantService.
SELECT id FROM tenants
WHERE status = 'pending_deletion'
  AND deletion_scheduled_at <= NOW()
ORDER BY deletion_scheduled_at, id
LIMIT @row_limit::int;

-- name: MarkTenantPurged :execrows
-- Completion marker. The status check makes a concurrent status change a
-- no-op instead of marking an operational tenant purged.
UPDATE tenants
SET purged_at = NOW()
WHERE id = @id::uuid
  AND status = 'deleted'
  AND purged_at IS NULL;

-- name: DeleteTenantSettings :execrows
-- Tenant purge: the deleted tenant's configuration has no reader left. The
-- primary key is tenant_id, so this affects at most one row and needs no
-- batching. The tenants row itself is kept for audit and incident review.
DELETE FROM tenant_settings WHERE tenant_id = $1;

-- name: TouchTenantIfUnchanged :execrows
-- The optimistic-concurrency gate for the tenants ETag. The row update and the
-- status/name change commit in one transaction, so exactly one concurrent
-- writer holding the same validator succeeds.
UPDATE tenants
SET updated_at = NOW()
WHERE id = @id
  AND updated_at = @expected_updated_at;
