-- name: GetTenantBySlug :one
SELECT * FROM tenants
WHERE slug = $1
  AND status = 'active';

-- name: GetTenantByID :one
SELECT * FROM tenants
WHERE id = $1
  AND status = 'active';

-- name: GetTenantByIDIncludingSuspended :one
-- Startup checks need existence independent of request eligibility so a
-- suspended configured/default tenant does not crash the process.
SELECT * FROM tenants
WHERE id = $1;

-- name: GetTenantBySlugIncludingSuspended :one
-- Startup checks need existence independent of request eligibility so a
-- suspended configured/default tenant does not crash the process.
SELECT * FROM tenants
WHERE slug = $1;
