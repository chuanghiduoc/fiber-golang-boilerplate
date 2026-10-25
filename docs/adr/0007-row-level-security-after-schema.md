# 0007. PostgreSQL RLS as defense in depth

- Status: Accepted
- Date: 2026-10-25

## Context

Tenant isolation is enforced only by `tenant_id` predicates in application
queries; one missing predicate leaks data across tenants. See
[PostgreSQL row security](https://www.postgresql.org/docs/current/ddl-rowsecurity.html).

## Decision

After the identity/tenancy schema rework (ADR 0004), every table with a
`tenant_id` column gets `ROW LEVEL SECURITY` enabled and forced, with one
`tenant_isolation` policy:

```sql
CREATE OR REPLACE FUNCTION app_tenant_visible(row_tenant BIGINT) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
  SELECT current_setting('app.rls_bypass', true) = 'on'
      OR row_tenant = NULLIF(current_setting('app.tenant_id', true), '')::BIGINT
$$;

ALTER TABLE files ENABLE ROW LEVEL SECURITY;
ALTER TABLE files FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON files
    USING (app_tenant_visible(tenant_id))
    WITH CHECK (app_tenant_visible(tenant_id));
```

`FORCE` makes the table owner subject to the policy too, so the owner role
works for the app. With neither setting the comparison is `NULL`, so a query
sees no rows (fail closed).

The settings are transaction-local:
`set_config('app.tenant_id', $1, true), set_config('app.rls_bypass', $2, true)`.
They are never session-level, because pooled connections are reused.

### Implementation notes

- `pkg/database.Scope{TenantID, Bypass}` and a `ScopeResolver` injected by
  `cmd/api`/`cmd/seed` (`app.RLSResolver`: `tenant.EffectiveID(ctx)` plus
  `database.RLSBypass(ctx)`). The package does not import `internal`.
- `database.TxManager.WithTx` sets the scope immediately after `BEGIN`, so
  every statement in a service transaction — including audit, events, and
  job enqueues — runs under one tenant.
- `database.Scoped` implements the sqlc `DBTX` surface for statements outside
  a transaction. Each statement runs as one pgx Batch:
  `BEGIN; SELECT set_config(...); <statement>; COMMIT`. One network round trip;
  a failed statement leaves the connection inside an aborted transaction, so
  `pgxpool` discards it on release instead of pooling it. `Query` returns rows
  whose `Close`/`Err` reads the `COMMIT` result and releases the connection.
  A query without a tenant (`Scope{}`) sees nothing.
- Every repository built in a module `build.go` receives the `Scoped` handle
  (`app.Infra.DB`), never the raw pool. The raw pool is left for health,
  shutdown, and the scheduler's own coordination tables
  (`scheduler_leases`/`scheduler_runs` have no tenant column).
- `package database.WithRLSBypass(ctx)` marks a deliberately cross-tenant
  operation instead of a separate job role. It is used by global job
  claim/heartbeat/lease recovery/finish/purge and enqueue-once lookups, the
  scheduler's job enqueue, the retention purges (identity tokens/sessions/
  invitations, files trash, notifications, webhooks, privacy exports),
  feature-flag snapshot loads, the public webhook receiver's source lookup,
  invitation accept/signup lookup, identity operations that touch a global
  user across tenants (logout-all, revoking sessions in every tenant, MFA
  revoke-others, account deletion, GDPR erasure/finalizer, tenant
  create/transition/settings), and audit writes that target another tenant.
  Never on an HTTP path serving one tenant.
- Child tables without `tenant_id` (`api_key_scopes`) stay unprotected; they
  are only reachable through their parent, whose policy runs first.

### Roles

The API, worker, and seed connect as a dedicated non-superuser role (`app`)
without `BYPASSRLS`. `cmd/migrate` connects as the schema owner
(`DB_MIGRATE_USERNAME`/`DB_MIGRATE_PASSWORD`, falling back to `DB_*`).
`deploy/postgres/database-roles.sql` creates the role and grants the schema,
sequences, functions, and default privileges for future migrations; docker
compose runs it from its init hook. `cmd/api` logs an error when the connected
role is a superuser or has `BYPASSRLS`, and refuses to start with one when
`APP_ENV=production`.

`cmd/migrate` verifies `relrowsecurity`, `relforcerowsecurity`, and the
`tenant_isolation` policy on every table that has a `tenant_id` column; its
test fails when that list drifts from the baseline.

## Consequences

- Every tenant-scoped query must run inside a transaction that sets the
  tenant, or through the scoped statement wrapper.
- Unique constraints and FK checks can still reveal existence; error mapping
  keeps returning 404 for cross-tenant ids.
- A missing `WithRLSBypass` on a global maintenance path fails closed (no rows
  or a `WITH CHECK` violation), which the integration suite catches.
