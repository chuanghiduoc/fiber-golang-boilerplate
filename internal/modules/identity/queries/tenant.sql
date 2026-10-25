-- Only trialing and active tenants serve requests ("operational").

-- name: GetOperationalTenantBySlug :one
SELECT * FROM tenants
WHERE slug = $1
  AND status IN ('trialing', 'active');

-- name: GetOperationalTenantByID :one
SELECT * FROM tenants
WHERE id = $1
  AND status IN ('trialing', 'active');

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
