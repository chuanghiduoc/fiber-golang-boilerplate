# 0007. PostgreSQL RLS as defense in depth

- Status: Accepted
- Date: 2026-10-25

## Context

Tenant isolation is enforced only by `tenant_id` predicates in application
queries; one missing predicate leaks data across tenants. See
[PostgreSQL row security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html).

## Decision

After the identity/tenancy schema rework (ADR 0004), enable `ROW LEVEL
SECURITY` with `FORCE` on tenant-owned tables. The application connects with a
non-owner role without `BYPASSRLS`; `TxManager` sets the tenant per
transaction with `set_config('app.tenant_id', $1, true)` (never session-level,
because pooled connections are reused). Platform jobs and `cmd/migrate` use
separate roles. Application predicates stay; RLS is the second line.

## Consequences

- Every tenant-scoped query must run inside a transaction that sets the tenant.
- Unique constraints and FK checks can still reveal existence; error mapping
  keeps returning 404 for cross-tenant ids.
