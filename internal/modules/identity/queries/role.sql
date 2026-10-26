-- RBAC roles. System roles have tenant_id IS NULL; tenant roles
-- belong to the request tenant. Reads include system roles plus the tenant's.

-- name: ListRolesCursor :many
SELECT r.id, r.tenant_id, r.scope, r.key, r.name, r.created_at, r.updated_at,
       ARRAY(
           SELECT rp.permission_key
           FROM role_permissions rp
           WHERE rp.role_id = r.id
           ORDER BY rp.permission_key
       )::text[] AS permissions
FROM roles r
WHERE r.scope = 'tenant'
  AND (r.tenant_id IS NULL OR r.tenant_id = @tenant_id::bigint)
  AND (r.created_at, r.id) < (@cursor_created_at::timestamptz, @cursor_id::bigint)
ORDER BY r.created_at DESC, r.id DESC
LIMIT @row_limit::int;

-- name: GetRole :one
SELECT r.id, r.tenant_id, r.scope, r.key, r.name, r.created_at, r.updated_at,
       ARRAY(
           SELECT rp.permission_key
           FROM role_permissions rp
           WHERE rp.role_id = r.id
           ORDER BY rp.permission_key
       )::text[] AS permissions
FROM roles r
WHERE r.id = @id
  AND (r.tenant_id IS NULL OR r.tenant_id = @tenant_id::bigint);

-- name: GetRoleByKey :one
SELECT r.id, r.tenant_id, r.scope, r.key, r.name, r.created_at, r.updated_at,
       ARRAY(
           SELECT rp.permission_key
           FROM role_permissions rp
           WHERE rp.role_id = r.id
           ORDER BY rp.permission_key
       )::text[] AS permissions
FROM roles r
WHERE r.key = @key
  AND r.scope = 'tenant'
  AND (r.tenant_id IS NULL OR r.tenant_id = @tenant_id::bigint)
ORDER BY r.tenant_id NULLS FIRST
LIMIT 1;

-- name: CreateRole :one
INSERT INTO roles (tenant_id, scope, key, name)
VALUES (@tenant_id, 'tenant', @key, @name)
RETURNING *;

-- name: UpdateRole :one
UPDATE roles
SET name = @name
WHERE id = @id AND tenant_id = @tenant_id
RETURNING *;

-- name: DeleteRole :execrows
DELETE FROM roles
WHERE id = @id AND tenant_id = @tenant_id;

-- name: UpsertRolePermission :exec
INSERT INTO role_permissions (role_id, permission_key)
VALUES (@role_id, @permission_key)
ON CONFLICT (role_id, permission_key) DO NOTHING;

-- name: PruneRolePermissions :exec
DELETE FROM role_permissions
WHERE role_id = @role_id
  AND permission_key <> ALL(@keep_keys::text[]);

-- name: RoleMemberIDs :many
SELECT user_id
FROM membership_roles
WHERE tenant_id = @tenant_id AND role_id = @role_id
ORDER BY user_id;

-- name: LockMembershipRoles :exec
-- Serializes role replacement for one membership, so concurrent requests
-- cannot delete each other's snapshot or leave a union of both role sets.
SELECT pg_advisory_xact_lock(
    hashtextextended('identity.membership_roles:' || @tenant_id::bigint, @user_id::bigint));

-- name: DeleteMemberRolesExcept :exec
DELETE FROM membership_roles
WHERE tenant_id = @tenant_id AND user_id = @user_id
  AND role_id <> ALL(@keep_role_ids::bigint[]);

-- name: DeleteMemberRoles :exec
DELETE FROM membership_roles
WHERE tenant_id = @tenant_id AND user_id = @user_id;

-- name: InsertMembershipRole :exec
INSERT INTO membership_roles (tenant_id, user_id, role_id)
VALUES (@tenant_id, @user_id, @role_id)
ON CONFLICT (tenant_id, user_id, role_id) DO NOTHING;

-- name: AssignPlatformRole :exec
INSERT INTO platform_role_assignments (user_id, role_id)
SELECT @user_id, r.id
FROM roles r
WHERE r.tenant_id IS NULL AND r.scope = 'platform' AND r.key = @role_key
ON CONFLICT (user_id, role_id) DO NOTHING;

-- name: LockPermissionSync :exec
-- Serializes catalog synchronization across replicas starting together.
SELECT pg_advisory_xact_lock(hashtextextended('identity.permission_sync', 0));

-- name: UpsertPermission :exec
INSERT INTO permissions (key, owner)
VALUES (@key, @owner)
ON CONFLICT (key) DO UPDATE SET owner = EXCLUDED.owner, updated_at = NOW();

-- name: PruneRolePermissionsOutsideCatalog :exec
DELETE FROM role_permissions
WHERE permission_key <> ALL(@keep_keys::text[]);

-- name: PrunePermissions :exec
DELETE FROM permissions
WHERE key <> ALL(@keep_keys::text[]);

-- name: UpsertSystemRole :one
INSERT INTO roles (tenant_id, scope, key, name)
VALUES (NULL, @scope, @key, @name)
ON CONFLICT (tenant_id, key) DO UPDATE SET scope = EXCLUDED.scope, name = EXCLUDED.name
RETURNING *;

-- name: DeleteTenantRoles :exec
-- Tenant purge: custom roles of the tenant. role_permissions cascade, and the
-- memberships (with their membership_roles) were deleted first.
DELETE FROM roles WHERE tenant_id = @tenant_id::bigint;

-- name: ListUserRoleAssignments :many
-- Export: the user's current membership role assignments in the request
-- tenant. Role changes are not historized, so created_at is when the current
-- assignment was granted.
SELECT r.key, mr.created_at
FROM membership_roles mr
JOIN roles r ON r.id = mr.role_id
WHERE mr.tenant_id = @tenant_id::bigint
  AND mr.user_id = @user_id::bigint
  AND r.scope = 'tenant'
ORDER BY mr.created_at, r.key;
