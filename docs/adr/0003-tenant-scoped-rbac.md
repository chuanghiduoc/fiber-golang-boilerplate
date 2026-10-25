# 0003. Tenant-scoped RBAC in the database

- Status: Accepted
- Date: 2026-10-25

## Context

Authorization had two hard-coded roles (`users.role CHECK ('user','admin')`),
no role tables, and "platform admin" was emulated as "admin of tenant 1".
Tenants could not define roles. Sources: [NIST RBAC](https://csrc.nist.gov/projects/role-based-access-control),
[NIST SP 800-162 (ABAC)](https://csrc.nist.gov/pubs/sp/800/162/upd2/final),
[Zanzibar](https://research.google/pubs/zanzibar-googles-consistent-global-authorization-system/),
[cedar-go](https://github.com/cedar-policy/cedar-go).

## Options considered

1. Tenant-scoped RBAC in the database, permission catalog in code.
2. Policy engine (Cedar, Casbin, OPA) now.
3. Keep fixed roles.

## Decision

Option 1. Permissions remain a code-owned catalog (reviewable, arch-tested)
synchronized into a `permissions` table. Roles live in the database: system
roles (`tenant_id IS NULL`, seeded, immutable) and tenant-defined roles, both
assigned to memberships. Platform-scope roles are separate assignments and are
the only way to obtain cross-tenant access. Resource checks go through
`authz.Authorize` policies (tenant, owner); the existing `Policy` interface is
the seam where Cedar can be plugged in later for ABAC.

## Consequences

- Schema: `permissions`, `roles`, `role_permissions`, `membership_roles`,
  `platform_role_assignments`; `users.role` is removed.
- The JWT no longer carries a role; effective permissions are resolved per
  membership and cached by the session store.
- The last-admin trigger becomes "a tenant keeps at least one owner".

## Implementation notes (P2b-2)

- Schema lives in the `identity` section of the init migration (ADR 0012):
  `permissions`, `roles`, `role_permissions`, `membership_roles`,
  `platform_role_assignments`; `memberships.role` is gone.
- System roles are `tenant_id IS NULL`: tenant `owner`, `admin`, `member`
  (seeded) and platform `platform_admin`. Owner and admin receive every
  tenant permission; member receives the catalog's listed grants;
  `platform_admin` receives every platform permission. Tenant roles may use
  known tenant permissions only.
- `permissions` and the system roles' grants are synchronized from
  `authz.Register` at API startup with concurrent-safe upserts; the catalog
  stays the source of truth.
- Effective permissions are the union of the membership's roles and the
  user's platform assignments, loaded into the session snapshot. The JWT no
  longer carries a role; a legacy claim still verifies and is ignored.
- `protect_last_owner` (on `memberships` and `membership_roles`) refuses
  removing the last active system owner of an operational tenant. Concurrent
  owner removals serialize on a per-tenant advisory lock shared with the
  service. The platform tenant create endpoint makes the caller the first
  owner.
- Platform tenant administration (`/platform/tenants`) and global feature-flag
  writes are platform permission checks; tenant admins manage their own roles
  under `/admin/roles` and replace member roles under
  `/admin/users/:id/roles`.
