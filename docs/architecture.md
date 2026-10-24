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
  apikeys      tenant-scoped API-key lifecycle (dto/repository/service/handler + adapter)
internal/contracts  narrow cross-module interfaces (identity/files admin + principal reader; stdlib only)
internal/platform
  apikey       key generation/hash, verifier, rotation grace, ApiKey middleware
  authz        subject/policies, permission catalog, ABAC + route middleware
  audit        audit recorder + mutating-request middleware
  jobs         durable PostgreSQL queue, worker, leases, retention
  session      token-version/session cache
  tenant       request tenant resolver + locals helpers (X-Tenant-Slug)
internal/platform/middleware   HTTP middleware (JWT, rate limit, logger, idempotency, ...)
pkg            reusable infrastructure (errors, response, database, cache, ...)
```

Request flow:

```
client
  -> app global middleware (CORS, security, request ID, metrics, logger,
     recovery, audit, timeout, idempotency where wired)
  -> /api/v1 tenant middleware (X-Tenant-Slug or default tenant)
  -> module route (JWT|ApiKey -> rate limit -> authz -> idempotency)
  -> handler -> service -> repository -> sqlc -> PostgreSQL
```

`tenant.Middleware` runs on the `/api/v1` group, before module auth. It
validates `[a-z0-9-]{1,63}`, resolves the slug (header or
`TENANT_DEFAULT_SLUG`), and stores `tenant_id`/`tenant_slug` in Fiber locals.
`JWTAuth` treats the JWT `tenant_id` claim as authoritative and rejects an
explicit header that resolves to a different tenant with 403.
`apikey.Authenticate` wraps the JWT handler: `Authorization: ApiKey ...`
verifies the credential, re-reads the owner principal, and installs the same
locals plus an `authz.Subject` limited to the key's scopes. Infrastructure
probes (`/healthz`, `/readyz`, `/metrics`) stay outside tenant resolution so
they never depend on the `tenants` table.

## Multi-tenant model

- One user belongs to exactly one tenant. `tenants.id` 1 with slug `default`
  is seeded by the baseline and preserves backward compatibility for header-less
  requests and rows written before multi-tenancy.
- `tenant_id` is present on `users`, `files`, and all identity/platform token,
  job, and audit tables, with `DEFAULT 1 REFERENCES tenants(id)`.
- Emails and Google IDs are unique per tenant; the last-admin trigger counts
  only admins in the affected tenant.
- Identity and files are tenant-isolated: every user/admin/stat and
  refresh/reset/verification query filters `tenant_id`; every file metadata
  read/write, list, admin list/stats, download, delete, and restore filters the
  request tenant. Repositories derive it from `tenant.EffectiveID(ctx)`; services
  verify returned tenant/owner. Cross-tenant ids or tokens behave as not found.
  Public login/register/forgot/reset/resend/verify/refresh use the request
  tenant; non-default tenants must send `X-Tenant-Slug` consistently.
- Durable jobs and audit events propagate the resolved tenant: enqueue writes
  `jobs.tenant_id` from the context, the worker restores it with
  `tenant.WithID` before calling a handler, and the audit middleware records
  the request tenant. Job retention and job metrics stay global.
- Google OAuth binds the tenant into `state` and the state cookie. The callback
  resolves that state tenant (and rejects suspended/unknown tenants) instead of
  trusting `X-Tenant-Slug`.
- JWTs without a `tenant_id` claim are rejected fail-closed. The session
  validator compares the claim with the stored user tenant and tenant status,
  so a suspended tenant cannot keep using an issued token beyond the 5-second
  session-cache TTL.
- API keys are tenant/user-bound rows in `api_keys` with only a SHA-256 hash at
  rest. Scopes are an explicit permission subset of the owner's role. Rotation
  sets `rotated_at`/`rotation_grace_until` on the old row and creates the new
  key in one transaction, so a grace-window key works until the deadline while
  revocation is immediate. Last use is touched on a throttled interval.
- ABAC subjects carry tenant, owner, explicit permissions, a cross-tenant flag,
  and attributes. `TenantPolicy`/`OwnerPolicy` conceal boundary violations as
  404; `Authorize` performs the RBAC pre-check first and is deny-overrides.
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

## Authorization

- Routes are protected by permissions, not role strings:
  `authz.Require(authz.Permission...)`. API-key subjects use their explicit
  scopes; JWT subjects derive the role catalog.
- Services add object-level checks through `authz.Authorize`, which combines an
  RBAC pre-check with a `PolicySet` over a `Resource`. `TenantPolicy` allows
  only the subject's tenant (or a `CrossTenant` platform subject) and
  `OwnerPolicy` only the resource owner. A boundary mismatch is a `Conceal`
  decision rendered as 404; an explicit deny is 403.
- `PolicySet` is deny-overrides and fails closed when empty. Handlers must not
  invent role checks; add a permission to the catalog and protect the route.
- Files use tenant+owner policies, admin files use the tenant policy only, and
  direct trusted callers without a subject retain the legacy repository-scoped
  checks.

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
- `api_keys` stores a globally unique SHA-256 hash and scopes as a JSONB array,
  with hash, tenant/user, and cursor indexes; the only global lookup is by hash,
  before a request tenant can be trusted.
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
- **Audit:** successful mutating requests are recorded with actor, request ID,
  IP, route, and the resolved tenant (`audit_events.tenant_id`, default 1).
  Explicit domain events use `audit.Recorder`; a zero `Event.TenantID` is
  resolved from the recording context.
- **Errors:** services return `*apperror.AppError` with a stable catalog code.
  HTTP renders RFC 9457 `application/problem+json`.
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
