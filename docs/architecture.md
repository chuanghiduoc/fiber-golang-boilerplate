# Architecture

This repository is a **modular monolith**: one deployable, module-owned HTTP
surfaces, and platform packages for cross-cutting mechanics. The goal is to
support many business modules without microservice overhead.

Related: [Data Model](data-model.md) (ER diagrams and table ownership),
[API Design](api-design.md), [Module Template](module-template.md),
[Operations](operations.md).

## Module map

```mermaid
flowchart TB
    subgraph root["Composition root"]
        api["cmd/api<br/>infra, lifecycle, shutdown"]
        boot["internal/bootstrap<br/>ordered module list + contract wiring"]
    end
    subgraph mods["internal/modules (one owner per table)"]
        identity
        files
        webhooks
        apikeys
        notifications
        features
        admin["admin<br/>(contracts only)"]
    end
    app["internal/app<br/>Fiber, global middleware, Module registry, Deps"]
    contracts["internal/contracts<br/>stdlib-only interfaces + event types"]
    platform["internal/platform<br/>authz, audit, jobs, events, tenant, ..."]
    pkg["pkg<br/>apperror, response, database, cache, ..."]

    api --> boot
    api --> app
    boot --> mods
    mods --> app
    mods --> contracts
    mods --> platform
    mods --> pkg
    app --> platform
    app --> contracts
    platform --> contracts
    platform --> pkg
    app --> pkg
```

Arrows are the **only** allowed import directions. `test/arch/imports_test.go`
(`make arch`) enforces them:

| Rule | Enforced by |
|---|---|
| A module never imports another module (including its generated `sqlc`). Use `internal/contracts` or a narrow exported interface. | module boundary walk |
| `internal/platform/*` and `pkg/*` never import `internal/modules/*`. | no-business-imports check |
| `internal/app` never imports modules; only `cmd/*` imports `internal/bootstrap`. | composition-root check |
| `internal/contracts` is stdlib-only (no repositories, DTOs, sqlc, storage, database, or session). | stdlib-only check |
| `admin` never imports identity/files, sqlc, `pkg/storage`, `pkg/database`, or the session store. | admin check |
| The deleted shared `internal/{handler,service,dto,repository}` and `internal/sqlc` packages cannot return. | legacy-package checks |
| Module SQL touches only owned tables (or platform tables). | `test/arch/table_ownership_test.go` |

### Packages

| Path | Responsibility |
|---|---|
| `cmd/api` | Infra setup, catalog registration, worker lifecycle, graceful shutdown |
| `cmd/migrate` | The only DDL entrypoint |
| `cmd/seed` | Standalone admin seeder (schema must already exist) |
| `config` | Env parsing, one file per concern (`app.go`, `auth.go`, `database.go`, `webhook.go`, ...); `Config.Validate` runs the section validators in a fixed order and reports the first error |
| `internal/app` | Fiber app, global middleware, infra routes, module registry + provider hooks, `app.Infra` |
| `internal/bootstrap` | The single ordered module list + contract cross-wiring (used by `cmd/api` and `test/arch`) |
| `internal/contracts/<provider>` | Stdlib-only contracts grouped by providing module: `identity` (admin, principal reader, notification recipient), `files` (admin), `notifications` (notifier), `webhooks` (event/dispatcher) |
| `internal/platform/*` | Cross-cutting mechanics (table below) |
| `internal/platform/middleware` | HTTP middleware: JWT, rate limit, logger, idempotency, ... |
| `pkg/*` | Reusable infrastructure: errors, response, database, cache, storage, email, webhook client, ... |

| Module | Owns |
|---|---|
| `identity` | Auth, users, sessions, tokens, tenants, admin seed (dto/repository/service/handler + adapter + queries/sqlc) |
| `files` | Upload, metadata, download, soft delete, restore (+ adapter + queries/sqlc) |
| `features` | Admin feature-flag catalogue surface (+ queries/sqlc) |
| `notifications` | In-app feed, channel preferences, durable email channel (+ queries/sqlc) |
| `admin` | Back-office facade (dto/service/handler, contracts-only) |
| `webhooks` | Outbound endpoints/deliveries, inbound sources/events, public HMAC receiver, dispatcher + delivery worker (+ queries/sqlc) |
| `apikeys` | Tenant-scoped API-key lifecycle (+ adapter) |

| Platform package | Responsibility |
|---|---|
| `apikey` | Key generation/hash, verifier, rotation grace, `ApiKey` middleware |
| `authz` | Subject/policies, permission catalog, ABAC + route middleware |
| `audit` | Audit recorder, `Guard`, fallback mutating-request middleware |
| `events` | Typed in-process bus + durable job-backed subscriptions |
| `features` | Compile-time flag catalog, evaluator, snapshot cache, DB store |
| `i18n` | Embedded `en`/`vi` catalogs, `Accept-Language` negotiation, localization |
| `jobs` | Durable PostgreSQL queue, worker, leases, retention |
| `requestctx` | Request ID + actor propagation through context |
| `scheduler` | PostgreSQL-leader interval/cron scheduler + unique run ledger |
| `session` | Token-version/session cache |
| `tenant` | Request tenant resolver + locals helpers (`X-Tenant-Slug`) |
| `tracing` | OpenTelemetry provider (no-op when disabled) |
| `versioning` | V1/V2 policy, `Accept-Version` assertions, deprecation headers |

## Request lifecycle

```mermaid
sequenceDiagram
    autonumber
    participant C as Client
    participant G as Global middleware
    participant V as /api versioning
    participant T as tenant.Middleware
    participant A as Authenticate
    participant R as Route chain
    participant H as Handler
    participant S as Service
    participant DB as Repository / sqlc / PostgreSQL

    C->>G: HTTP request
    Note over G: Recovery, CORS, security headers,<br/>RequestID, i18n, tracing, metrics, logger,<br/>Recovery, JSON body limit, audit fallback, timeout
    G->>V: versioning.Guard on /api (unknown vN: 406)
    Note over V: group versioning.Middleware:<br/>Accept-Version must match path (406)
    V->>T: resolve X-Tenant-Slug, ?tenant= (browser auth paths) or default
    Note over T: unknown/suspended tenant: 404
    T->>A: per-IP auth limiter, then ApiKey or Bearer JWT
    Note over A: 429, 401 bad credential,<br/>403 tenant mismatch
    A->>R: route limiter (strict/normal/relaxed)
    Note over R: authz.Require (403) then optional feature gate (404)<br/>then Idempotency (replay or 409) then audit.Guard (503)
    R->>H: c.Next()
    H->>S: validated DTO
    S->>DB: query scoped by tenant.EffectiveID(ctx)
    DB-->>S: rows
    S-->>H: result or *apperror.AppError
    H-->>C: response.* JSON, or RFC 9457 problem via the error handler
```

| Stage | Mounted on | Notes |
|---|---|---|
| Global middleware | every route, in the order shown | `i18n.Middleware` runs right after request ID so every response, errors included, carries the negotiated locale. `audit.Middleware` is a fallback and skips routes guarded by `audit.Guard`. |
| `versioning.Guard` | `/api` | Rejects unknown `/api/vN` before routing. |
| `versioning.Middleware` + `tenant.Middleware` | `/api/v1` and `/api/v2` groups | v2 exists only when a module implements `app.V2Module`. Probes (`/healthz`, `/readyz`, `/metrics`) stay outside tenant resolution. |
| `deps.Authenticate()` | module group or route | Per-IP `auth` limiter first, then `apikey.Authenticate` wrapping `JWTAuth`. |
| Route chain | each route | `limiter → authz.Require → [features gate] → deps.Idempotency → audit.Guard → handler`. Idempotency replays short-circuit before the guard, so replays are never re-audited. |

### Tenant and credential resolution

- `tenant.Middleware` validates `[a-z0-9-]{1,63}` and resolves the slug: header;
  validated `?tenant=` fallback only on `/auth/google`, `/auth/reset-password`,
  and `/auth/verify-email`; otherwise `TENANT_DEFAULT_SLUG`
  (`TENANT_REQUIRE_HEADER=true` disables the default). It stores
  `tenant_id`/`tenant_slug` in Fiber locals and the request context.
- The Google OAuth callback is exempt so require-header mode cannot reject it
  before the handler validates the signed `state`; the handler installs the
  state tenant itself.
- `JWTAuth` treats the token's `tenant_id` claim as authoritative, rejects a
  non-positive claim (401), and rejects an explicit header that resolves to a
  different tenant (403). It clears `tenant_slug` when the claim overrides a
  header-less default resolution.
- `Authorization: ApiKey ...` verifies the credential, re-reads the owner
  principal, and installs the same locals plus an `authz.Subject` limited to
  the key's scopes. See [API Design](api-design.md#authentication-and-authorization).
- The public webhook receiver (`POST /webhooks/inbound/:id`) mounts auth per
  route instead of on the group, so it bypasses JWT and tenant-header
  selection. The source row supplies the tenant; the source HMAC authenticates
  the caller.

## Cross-module communication

```mermaid
flowchart LR
    subgraph sync["1 - Synchronous contract call"]
        adm[admin] -->|IdentityAdmin / FilesAdmin| ad1[identity + files adapters]
        ak[apikeys] -->|PrincipalReader| ad2[identity adapter]
        nt[notifications] -->|NotificationRecipientLookup| ad2
    end
    subgraph outbox["2 - Transactional outbox"]
        prod["identity / files service<br/>business tx"] -->|DispatchTx in same tx| j1[("jobs<br/>webhook.dispatch")]
        j1 -->|worker| wh["webhooks fan-out<br/>delivery rows + webhook.delivery jobs"]
    end
    subgraph bus["3 - Domain events"]
        pub["producer"] -->|PublishTx| j2[("jobs<br/>one per durable subscriber")]
        pub -->|Publish / PublishLocal| loc["Subscribe handlers<br/>(synchronous, in-process)"]
        j2 -->|worker| dur["SubscribeDurable handler"]
    end
```

| Mechanism | Use when | Guarantees |
|---|---|---|
| **Contract** (`internal/contracts/<provider>`) | A module must read or act on another module's data now. | Stdlib-only interface; provider implements it in its `adapter` package; `internal/bootstrap` passes it to the consumer. No shared transaction. |
| **Outbox** (`jobs` table) | A side effect must exist if and only if the business change commits. | Job row commits with the business row (`EnqueueTx`/`EnqueueOnceTx`); delivery is at-least-once, so handlers are idempotent. |
| **Durable event** (`events.PublishTx` + `SubscribeDurable`) | Several independent reactions to one fact. | One job per subscription, key `event:<name>:<id>:<handler>:<tenantId>`; at-least-once. |
| **Local event** (`Subscribe`) | In-process reactions that may fail with the request. | Runs synchronously before commit; panics recovered per handler, errors combined with `errors.Join`. Keep irreversible side effects out. |

Contract wiring performed by `internal/bootstrap`:

```mermaid
flowchart LR
    identity -->|IdentityAdmin| admin
    files -->|FilesAdmin| admin
    identity -->|PrincipalReader| apikeys
    identity -->|NotificationRecipientLookup| notifications
    identity -->|TenantActive func| webhooks
    webhooks -->|Dispatcher| identity
    webhooks -->|Dispatcher| files
    identity -->|Sessions, TenantResolver| deps["app.Deps"]
    apikeys -->|Verifier| deps
    webhooks -->|Dispatcher| deps
```

Arrows read "provider supplies X to consumer". Notifications exposes
`notifications.Notifier` (`internal/contracts/notifications`) for other
modules and never imports identity or reads `users`; no other module consumes
it yet.

## Multi-tenant model

One user belongs to exactly one tenant. Tenant `1` (`default`) is seeded by
the baseline and keeps header-less local/test requests and pre-tenancy rows
working; production sets `TENANT_REQUIRE_HEADER=true`. Startup checks tenant
**existence** regardless of status, so a suspended default tenant starts
normally and its requests return 404.

| Area | Isolation rule |
|---|---|
| Schema | Every tenant-owned table has `tenant_id … DEFAULT 1 REFERENCES tenants(id)` and composite `(tenant_id, id)` child FKs. See [Data Model](data-model.md). |
| Identity | Every user/admin/stat and refresh/reset/verification query filters `tenant_id`. Emails and Google IDs are unique per tenant; the last-admin trigger counts only the affected tenant. Public auth flows use the request tenant; reset/verification links carry `?tenant=<slug>` (header wins). |
| Files | Every metadata read/write, list, admin list/stats, download, delete, and restore filters the request tenant. Services re-check the returned tenant/owner. New blobs use `<tenantID>/<userID>/<uuid>.<ext>`; old rows keep `<userID>/<uuid>`. |
| Notifications | Every query filters `(tenant_id, user_id)`; another user's or tenant's id is not found. |
| Webhooks | Endpoints, secrets, deliveries, attempts, sources, and inbound events all lead with `tenant_id`. The only unscoped lookup is the public receiver's source-by-ID, which then authenticates with HMAC and installs the source tenant. |
| API keys | Tenant/user-bound rows with only a SHA-256 hash at rest; scopes are a subset of the owner's role. Rotation marks the old row and creates the new key in one transaction (grace window); revocation is immediate; last use is touched on a throttled interval. |
| Feature flags | Targets are tenant-owned (`tenant.EffectiveID(ctx)`); a composite FK keeps user targets in their tenant. Definitions, defaults, and kill switches are process-wide. See [Feature Flags](feature-flags.md). |
| Jobs, events, audit | Enqueue writes `jobs.tenant_id` from the context; the worker restores it with `tenant.WithID`. Event metadata carries tenant/request/actor; audit rows record the request tenant. Retention and metrics stay global. |
| Scheduler | One global leader. Each schedule chooses a tenant in its spec; the job stores it. `scheduler_leases`/`scheduler_runs` have no tenant column. |
| OAuth | The tenant is bound into `state` and the state cookie; the callback rejects unknown/suspended tenants instead of trusting `X-Tenant-Slug`. |
| Sessions | Tokens without a positive `tenant_id` claim are rejected. The session validator compares the claim with the stored user tenant and tenant status, so a suspension takes effect within the 5-second session-cache TTL. |
| Idempotency | Cache keys include tenant ID/slug, method, path, and caller, so a replayed `201` cannot cross tenants. |
| Logout | Revokes refresh tokens by possession, so a valid token is deleted even without the header. |

Repositories derive the tenant from `tenant.EffectiveID(ctx)`; the tenant-1
fallback is only for jobs, internal callers, and tests. Cross-tenant ids or
tokens behave as not found. Row-level security and per-tenant limits remain
future work.

## Authorization

- Routes are protected by permissions, never role strings:
  `authz.Require(authz.Permission...)` (every route; enforced by `test/arch`).
  API-key subjects use their explicit scopes; JWT subjects derive them from the
  role catalog.
- Services add object-level checks with `authz.Authorize`: an RBAC pre-check,
  then a deny-overrides `PolicySet` over a `Resource` that fails closed when
  empty. `TenantPolicy` allows only the subject's tenant (or a `CrossTenant`
  platform subject); `OwnerPolicy` only the owner.

| Decision | HTTP |
|---|---|
| Missing permission (`authz.Require`) or explicit deny | `403 permission.denied` |
| Boundary mismatch (`Conceal`, other tenant/owner) | `404` |

Files use tenant + owner policies; admin files use the tenant policy only;
direct trusted callers without a subject keep repository-scoped checks.

## Data and transactions

- One transaction scopes one module/aggregate (`database.TxManager.WithTx`).
  Never span modules in a transaction; use jobs or events.
- The `jobs` table is the durable outbox. `EnqueueTx` commits the job with the
  business row; `EnqueueOnce`/`EnqueueOnceTx` add a partial unique
  `(tenant_id, idempotency_key)` for producer retries and event fan-out.
- The scheduler writes its `scheduler_runs` row and job in one transaction;
  the unique `(schedule, scheduled_for)` ledger is the idempotency boundary
  across leader changes, and the lease row is the only cross-replica
  coordination point.
- Email delivery and durable events are at-least-once; handlers must be
  idempotent. Terminal jobs clear their payload and are purged after a global
  retention window. Email payloads are AES-256-GCM sealed with a key derived
  from `JWT_SECRET` (HKDF); rotate the secret only after draining the queue.
- **Outbound webhooks.** Identity registration, file upload/delete (user and
  admin), and file restore emit `user.registered`, `file.uploaded`,
  `file.deleted`, and `file.restored` with `DispatchTx` inside the mutation's
  transaction: one `webhook.dispatch` job per event, so an enqueue failure
  rolls the mutation back (fail-closed; only an unwired deployment falls back
  to a logged post-commit dispatch). The dispatch job fans out to matching
  endpoints, committing each delivery row with its first `webhook.delivery`
  job. Deliveries snapshot URL, payload, event timestamp, max attempts, and
  sealed secret. Retries schedule new jobs (honoring `Retry-After`) and every
  attempt is kept. Receivers deduplicate on `X-Webhook-Delivery`.
- **Inbound webhooks.** The tenant comes from the source row; HMAC covers
  `<X-Webhook-Id>.<timestamp>.<X-Webhook-Event>.<raw-body>`; deduplication is
  unique per `(tenant, source, external_event_id)`. See [Webhooks](webhooks.md).
- **Notifications.** With the email channel enabled, the in-app row and its
  email job commit or roll back together. A suppressed notification (both
  channels disabled) writes nothing. Without transactional sender support the
  service falls back to a post-insert send; production wiring always uses the
  queued transactional sender.

## Schema ownership

- Only `cmd/migrate` applies DDL. The API and seed assume the schema exists;
  `cmd/migrate` also verifies required tenant tables/columns/indexes and that
  the `default` tenant exists with id 1.
- Fresh installs use one baseline: `migrations/000001_init_schema.{up,down}.sql`.
- Once a database is deployed, add timestamped migrations
  (`make migrate-create module=<owner> name=<purpose>`); never edit a shipped
  migration. Legacy databases keep their recorded history (see README).
- `feature_flags` is global configuration; `scheduler_leases` and
  `scheduler_runs` are tenant-independent with no foreign keys. Everything
  else is tenant-owned with tenant-leading indexes.
- `api_keys.key_hash` is globally unique; it is the only lookup made before a
  request tenant can be trusted.
- `files.storage_path` is the authoritative blob key; moving a blob without
  updating it (or vice versa) breaks the file.

Table-by-table details, keys, and FK actions: [Data Model](data-model.md).

## Cross-cutting conventions

| Concern | Convention |
|---|---|
| Authorization | Module permissions live in `catalog/permissions.go` and register through `app.PermissionProvider`; only shared back-office permissions stay in `internal/platform/authz`. Notification routes use `notifications:read`/`notifications:write`; sending to another user needs admin-only `notifications:send`. Legacy exceptions: [Module Template](module-template.md#legacy-exceptions-do-not-copy). |
| Audit | Privileged mutations are fail-closed. Tier 1 services (identity registration/profile/password/delete, admin role/ban/unban, admin file delete/restore, API-key lifecycle) call `audit.TxRecorder.RecordTx` in the mutation transaction. Tier 2 routes use `audit.Guard`: an `outcome=started` intent row before the handler, completed as succeeded/failed/denied; a required guard returns `503 system.audit_unavailable` when the intent cannot be stored. Rows carry actor type/role, source, request ID, IP, route, and tenant. `audit.NewServiceEvent` enriches domain events from the context. |
| Errors | `*apperror.AppError` with a stable catalog code, rendered as RFC 9457 `application/problem+json`. With `Deps.I18n`, titles and validation messages follow `Accept-Language`; `DetailKey` opts a detail in. A nil localizer keeps English. |
| i18n | `en`/`vi` catalogs must keep identical keys and `{placeholders}` (`i18n.New` fails otherwise). `vi-VN` truncates to `vi`; unsupported languages fall back to `I18N_DEFAULT_LOCALE`. |
| API versions | `/api/v1` always; opt-in `/api/v2` via `app.V2Module`. `Accept-Version` must match the path (406 `api.version_unsupported`). Responses carry `API-Version`, `Vary: Accept-Version`, and configured deprecation metadata. Swagger stays on v1. |
| Idempotency | Opt-in `Idempotency-Key` on mutating routes, cache-backed, 24h replay, body-bound for non-multipart requests. Secret-returning creates are excluded so plaintext secrets never enter the shared cache. |
| Feature flags | Module-declared catalog (`catalog/flags.go`, `app.FlagProvider`), precedence force-off > kill switch > user > tenant > database default > environment > compiled default. Admin writes invalidate the local snapshot. |
| Observability | Request IDs, structured `slog`, Prometheus HTTP/job/event/scheduler/audit metrics, readiness checks for DB/cache. Labels stay bounded (`event`, `schedule`, `outcome`), never tenant IDs. |

## Module registration and catalogs

```mermaid
flowchart TD
    cfg["config.Load<br/>(FEATURE_FLAGS syntax only)"] --> infra["Infra: tracing, pool, storage, cache,<br/>flag evaluator, email, tx, audit, job queue/worker,<br/>event bus, queued email sender"]
    infra --> build["bootstrap.Build(infra)<br/>every module's Build + contract wiring"]
    build --> cat["app.RegisterCatalogs<br/>permissions, error codes, flags"]
    cat --> keys["cfg.FeatureFlags.ValidateKnownKeys<br/>unknown FEATURE_FLAGS keys fail here"]
    keys --> i18n["i18n.New(locale, app.LocaleSources)<br/>merge + parity check"]
    i18n --> startup["Modules.Startup<br/>tenant existence + admin seed"]
    startup --> appnew["app.New + app.Register<br/>routes, jobs, events, schedules"]
    appnew --> run["email.send handlers, scheduler,<br/>worker, Modules.Background, HTTP listen"]
```

- Catalog registries (`authz`, `apperror`, `features`) are process-wide
  copy-on-write snapshots behind an `atomic.Pointer`, so the per-request read
  path takes no lock. Registration fails on invalid identifiers, unknown roles,
  or a key owned by another module; identical re-registration by the same
  owner is a no-op so tests can build the app repeatedly.
- Platform-wide entries stay central: `permission.denied`, `feature.disabled`,
  `rate_limit.exceeded`, `api.version_unsupported`, `system.*`, generic status
  titles, validation messages, and the shared back-office permissions
  (`admin:users:*`, `admin:files:*`, `admin:stats:read`).
- `internal/bootstrap` is the only non-`cmd` package that imports several
  modules. `test/arch` uses the same `bootstrap.Build`, so route authorization,
  audit coverage, group-middleware overlap, and SQL table ownership are
  checked for every registered module.

## Strangler status

The migration is complete. Every module owns its DTOs, repositories,
services, and handlers under `internal/modules/<name>`; each module with SQL
owns its `queries/` and generated `sqlc/` package (one `sqlc.yaml` block per
module; `admin` is contract-only). There is no shared legacy layer, no shared
generated package, and no cross-module SQL: identity and files each own their
half of the admin dashboard stats (`AdminUserStats` / `AdminFileStats`).

## Extension checklist

Use [`scripts/new-module.sh`](../scripts/new-module.sh) and the
[Module Template](module-template.md). Short form:

1. `scripts/new-module.sh <name>`: module.go (`Build` + providers), catalog
   (permissions, error codes, en/vi strings), layers, `queries/`, sqlc block,
   module test, README.
2. Add the module to `internal/bootstrap/modules.go` (the only central edit).
3. Add tables: update the baseline for fresh installs, or
   `make migrate-create module=<name> name=<purpose>` for deployed DBs.
4. Add handler/service/repository/DTO and protected routes in the module.
5. Implement the optional `app.EventProvider`, `app.JobProvider`,
   `app.ScheduleProvider`, or `app.FlagProvider` hooks instead of wiring work
   imperatively.
6. Add unit + integration tests; run `make check`, `go test -race ./...`, and
   `make swagger`.
7. For durable work, enqueue a job transactionally; never use `async.Go` for
   work that must survive a restart.
