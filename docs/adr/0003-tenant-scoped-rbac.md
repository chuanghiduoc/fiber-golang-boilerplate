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
