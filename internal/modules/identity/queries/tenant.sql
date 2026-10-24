-- name: GetTenantBySlug :one
SELECT * FROM tenants
WHERE slug = $1
  AND status = 'active';

-- name: GetTenantByID :one
SELECT * FROM tenants
WHERE id = $1
  AND status = 'active';
