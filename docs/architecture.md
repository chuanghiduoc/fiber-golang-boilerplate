# Architecture

This repository is a **modular monolith**: one deployable, module-owned HTTP
surfaces, and platform packages for cross-cutting mechanics. The goal is to
support many business modules without microservice overhead.

## Runtime shape

```
cmd/api        bootstrap, DI, worker lifecycle, graceful shutdown
cmd/migrate    the only DDL entrypoint
cmd/seed       standalone admin seeder (schema must already exist)

internal/app   composition root: Fiber, middleware, infra routes, module registry
internal/modules
  identity     auth, users, sessions, tokens, admin seed
               (dto/repository/service/handler + adapter + queries + generated sqlc)
  files        upload, metadata, download, soft delete, restore
               (dto/repository/service/handler + adapter + queries + generated sqlc)
  admin        back-office facade (dto/service/handler, contracts-only)
internal/contracts  narrow cross-module identity/files admin interfaces (stdlib only)
internal/platform
  authz        permission catalog + route middleware
  audit        audit recorder + mutating-request middleware
  i18n         embedded en/vi catalogs, Accept-Language negotiation, localization
  jobs         durable PostgreSQL queue, worker, leases, retention
  session      token-version/session cache
  tenant       request tenant resolver + locals helpers (X-Tenant-Slug)
  versioning   V1/V2 policy, Accept-Version assertions, deprecation headers
internal/platform/middleware   HTTP middleware (JWT, rate limit, logger, idempotency, ...)
pkg            reusable infrastructure (errors, response, database, cache, ...)
```

Request flow:

```
client
  -> app global middleware (CORS, security, request ID, i18n, tracing, metrics,
     logger, recovery, audit, timeout, idempotency where wired)
  -> /api version guard (unknown /api/vN -> 406)
  -> /api/vN version middleware (Accept-Version, API-Version, deprecation)
  -> tenant middleware (X-Tenant-Slug, browser auth query, or default)
  -> module route (JWT -> rate limit -> authz -> idempotency)
  -> handler -> service -> repository -> sqlc -> PostgreSQL
```

`i18n.Middleware` runs immediately after request ID so every response,
including errors, carries the negotiated locale. `pkg/apperror` uses the
request locale for catalog titles and validation messages. `versioning.Guard`
runs on `/api` before the version groups; `versioning.Middleware` is mounted on
each group. The v2 group exists only when a module implements `app.V2Module`.

`tenant.Middleware` runs on the `/api/v1` and `/api/v2` groups, before module
JWTs. It validates `[a-z0-9-]{1,63}`, resolves the slug (header; validated
`?tenant=` fallback on `/auth/google`, `/auth/reset-password`, and
`/auth/verify-email`; otherwise `TENANT_DEFAULT_SLUG`), and stores
`tenant_id`/`tenant_slug` in Fiber locals and the request context. The Google
OAuth callback is exempt so `TENANT_REQUIRE_HEADER=true` cannot reject it
before the handler validates the signed `state`; the handler installs the
state tenant itself. `JWTAuth` treats the JWT `tenant_id` claim as
authoritative, rejects a non-positive claim, and rejects an explicit header
that resolves to a different tenant with 403. It clears `tenant_slug` when a
claim overrides a header-less default resolution rather than leaving a
misleading slug. Infrastructure probes (`/healthz`, `/readyz`, `/metrics`)
stay outside tenant resolution so they never depend on the `tenants` table.

## Multi-tenant model

- One user belongs to exactly one tenant. `tenants.id` 1 with slug `default`
  is seeded by the baseline and preserves backward compatibility for header-less
  local/test requests and rows written before multi-tenancy. Production sets
  `TENANT_REQUIRE_HEADER=true`, so every HTTP write selects a tenant explicitly.
  Startup checks tenant existence regardless of status; a suspended default
  tenant starts normally and its requests return 404.
- `tenant_id` is present on `users`, `files`, and all identity/platform token,
  job, and audit tables, with `DEFAULT 1 REFERENCES tenants(id)`.
- Emails and Google IDs are unique per tenant; the last-admin trigger counts
  only admins in the affected tenant.
- Identity and files are tenant-isolated: every user/admin/stat and
  refresh/reset/verification query filters `tenant_id`; every file metadata
  read/write, list, admin list/stats, download, delete, and restore filters the
  request tenant. Repositories derive it from `tenant.EffectiveID(ctx)` (the
  tenant-1 fallback is only for jobs/internal callers/tests without a request
  tenant); services verify returned tenant/owner. Cross-tenant ids or tokens
  behave as not found.
  Public login/register/forgot/reset/resend/verify/refresh use the request
  tenant; non-default tenants must send `X-Tenant-Slug` consistently on API
  calls. Reset/verification links include `?tenant=<slug>`, and browser-facing
  auth paths accept that validated fallback; header values take precedence.
- Durable jobs and audit events propagate the resolved tenant: enqueue writes
  `jobs.tenant_id` from the context, the worker restores it with
  `tenant.WithID` before calling a handler, and the audit middleware records
  the request tenant. Job retention and job metrics stay global.
- Google OAuth binds the tenant into `state` and the state cookie. The callback
  resolves that state tenant (and rejects suspended/unknown tenants) instead of
  trusting `X-Tenant-Slug`; tenant middleware exempts the callback path so
  require-header mode does not 404 it first.
- JWTs without a `tenant_id` claim, or with a non-positive claim, are rejected
  fail-closed. The session validator compares the claim with the stored user
  tenant and tenant status, so a suspended tenant cannot keep using an issued
  token beyond the 5-second session-cache TTL.
- Idempotency cache keys include the tenant ID/slug as well as method, path,
  and caller, so a replayed `201` cannot cross tenants. Logout revokes refresh
  tokens by possession rather than request tenant, so a valid token is deleted
  even when the header is omitted.
- New blobs use `<tenantID>/<userID>/<uuid>.<ext>`; existing rows keep their
  old `<userID>/<uuid>` path. RLS and per-tenant limits remain future work.


## Dependency rules

- A module must not import another module's internals. Use an exported
  contract in `internal/contracts` or a narrow interface. Contracts are
  stdlib-only and never import modules, repositories, DTOs, sqlc, storage,
  database, or session packages.
- `internal/platform/*` and `pkg/*` must not import business modules.
- One module owns each table. Cross-module reads go through a read model or
  contract, never another module's repository.
- Module DTOs, repositories, services, and handlers stay inside the owning
  module. Each module that has SQL owns its `queries/` and generated `sqlc/`
  package (`admin` is contract-only); a module must never import another
  module's generated SQL types.
- Modules must not import the deleted shared legacy handler/service/DTO/
  repository packages; the architecture test enforces this so the layer cannot
  return.
- Enforced by `test/arch/imports_test.go` (`make arch`).

## Data and transactions

- One transaction scopes one module/aggregate. Do not span modules in a
  transaction; use jobs/events instead.
- The `jobs` table is the durable outbox. Enqueue with `Queue.EnqueueTx` so the
  job commits atomically with the business row. Enqueue stores the request
  tenant in `jobs.tenant_id`; claims return it and the worker restores it in
  the handler context, so tenant-scoped side effects are possible after the
  request ends.
- Email delivery is at-least-once. Handlers must be idempotent. Terminal jobs
  clear their payload and are purged after retention; retention is global and
  ignores tenant boundaries.
- Email payloads are encrypted at rest with AES-256-GCM; keys are derived from
  `JWT_SECRET` via HKDF. Rotate the secret only after draining the queue.

## Schema ownership

- Only `cmd/migrate` applies DDL. The API and seed assume the schema exists.
- Fresh installs use one baseline: `migrations/000001_init_schema.{up,down}.sql`.
- Once a database is deployed, add numbered migrations; never edit a shipped
  migration. Legacy databases must keep their recorded history (see README).
- Tenant-owned tables carry `tenant_id` with a default of 1 and a foreign key to
  `tenants`; tenant-leading indexes back scoped queries. The baseline seeds the
  `default` tenant as row 1, so existing rows and header-less requests remain on
  the compatibility tenant.
- File blobs are external to PostgreSQL but follow the row's tenant: new keys
  are `<tenantID>/<userID>/<uuid>.<ext>`, while `files.storage_path` is the
  authoritative key for downloads. Moving a blob without updating that column
  (or vice versa) breaks the file.
- `cmd/migrate` verifies required tenant tables/columns/indexes and that the
  default tenant exists with id 1.

## Cross-cutting conventions

- **Authorization:** permissions live in `internal/platform/authz`; every route
  uses `authz.Require`. Roles are an implementation detail of the permission
  matrix. Current legacy exceptions (`files` routes, some identity handler
  checks) are listed in the [Module Template](module-template.md#legacy-exceptions-do-not-copy).
- **Audit:** every privileged mutation is audited fail-closed. Tier 1 services
  (identity registration/profile/password/delete, admin role/ban/unban, admin
  file delete/restore) call `audit.TxRecorder.RecordTx` inside the same
  transaction as the mutation; the mutation rolls back when the audit insert
  fails. Tier 2 routes use `audit.Guard`, which writes an `outcome=started`
  intent row before the handler and completes it as succeeded/failed/denied
  afterwards; required guards return `503 system.audit_unavailable` when the
  intent cannot be stored. Rows carry actor type/role, source, request ID, IP,
  route, and the resolved tenant (`audit_events.tenant_id`, default 1).
  `audit.NewServiceEvent` enriches explicit domain events from the request
  context; a zero `Event.TenantID` is resolved from the recording context.
- **Errors:** services return `*apperror.AppError` with a stable catalog code.
  HTTP renders RFC 9457 `application/problem+json`. With a localizer installed
  (`Deps.I18n`), titles and validation messages follow `Accept-Language`;
  `DetailKey` opts a detail into translation without changing code/type/status.
  A nil localizer keeps the literal English output.
- **i18n:** embedded `en`/`vi` catalogs must keep identical keys and
  `{placeholders}`; `i18n.New` fails otherwise. Locale tags are truncated to the
  supported base (for example `vi-VN` → `vi`) and unsupported languages fall
  back to `I18N_DEFAULT_LOCALE`.
- **API versions:** path routing under `/api/v1`, with opt-in `/api/v2` through
  `app.V2Module`. Optional `Accept-Version` must match the path (406
  `api.version_unsupported` otherwise). Version middleware adds `API-Version`,
  `Vary: Accept-Version`, and configured deprecation metadata; the guard
  rejects unknown `/api/vN` before routing. Swagger stays on v1.
- **Idempotency:** opt-in `Idempotency-Key` on mutating routes, cache-backed,
  24h replay, body-bound for non-multipart requests.
- **Observability:** request IDs, structured `slog`, Prometheus HTTP/job/audit
  metrics, readiness checks for DB/cache.

## Strangler status

The migration is complete. `identity`, `files`, and `admin` own their DTOs,
repositories, services, and HTTP handlers under `internal/modules/<name>`;
there are no shared legacy handler/service/DTO/repository packages or
compatibility aliases.

The `admin` module depends on `internal/contracts.IdentityAdmin` and
`internal/contracts.FilesAdmin`; identity and files implement those contracts
with adapters in their own `adapter` packages. Admin never imports another
module, its repository, generated SQL, storage, database transactions, or
session store. Identity and files each own their half of the dashboard stats
(`AdminUserStats` / `AdminFileStats`), so no cross-module SQL remains.

`identity` and `files` own module-scoped `queries/` and generated `sqlc/`
packages; `sqlc.yaml` has one block per module. There is no shared generated
package, and no module imports another module's sqlc types. New code should
start in `internal/modules/<name>/` and use contracts for cross-module reads.

## Extension checklist

Use [`scripts/new-module.sh`](../scripts/new-module.sh) and the canonical
[Module Template](module-template.md) for the full layout, skeleton, and
ownership rules. Short form:

1. Create the module, its `queries/` directory and sqlc block, plus tables;
   update the baseline for fresh installs (or add a numbered migration for
   deployed DBs).
2. Add permissions and audit actions.
3. Add handler/service/repository/DTO in the module directory.
4. Register routes and the module in `cmd/api/main.go`.
5. Add unit + integration tests; run `make check`, `go test -race ./...`, and
   `make swagger`.
6. For durable work, enqueue a job transactionally; never use `async.Go` for
   work that must survive a restart.
