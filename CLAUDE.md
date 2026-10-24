# CLAUDE.md

Guidance for agents working in this repository. Detail lives in `docs/`; this
file keeps the invariants you must not break.

## Build & Run

```bash
make build                 # bin/server + bin/migrate (.exe on Windows)
make migrate-run           # apply pending migrations via cmd/migrate (no migrate-up)
make migrate-down CONFIRM=yes  # drops the whole schema; disposable DBs only
make run                   # migrate, then run the API locally
make seed                  # migrate (dependency) then seed the admin user
make test                  # unit tests
make test-race             # unit tests with -race (needs cgo)
make test-integration      # integration tests (Docker/testcontainers or TEST_DATABASE_URL)
make check                 # vet + lint + arch tests + unit tests
make lint                  # golangci-lint v2
make docker-run            # Compose: migrate job -> API
make docker-down
make sqlc-generate
make swagger
```

Production build: `CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o server ./cmd/api`.
`swag`/`air` run via `go run` at versions pinned in the Makefile
(`SWAG_VERSION`/`SQLC_VERSION` match CI). `scripts/rename-module.sh <path>`
reads the current path from `go list -m` and rewrites tracked text files with
perl (needs go + perl).

CI builds both binaries; its `Migrations` job applies the baseline up/down on a
fresh PostgreSQL service and runs `cmd/migrate` twice, and its `Manifests` job
runs actionlint, kustomize, and kubeconform. Reproduce the fresh-DB check
against a disposable database before merging. Kubernetes: follow
`deploy/k8s/README.md` (migration Job first, then the app kustomization);
`.github/workflows/release.yml` builds multi-arch GHCR images on `v*` tags.

## Architecture

**Modular monolith.** One deployable; modules own route tables; platform
packages own cross-cutting mechanics.

```mermaid
flowchart LR
    cmd["cmd/api"] --> boot["internal/bootstrap"]
    boot --> mods["internal/modules/*"]
    cmd --> app["internal/app"]
    mods --> app
    mods --> contracts["internal/contracts<br/>(stdlib only)"]
    mods --> plat["internal/platform"]
    mods --> pkg["pkg"]
    app --> plat
    plat --> pkg
```

Arrows are the only allowed import directions; a module never imports another
module, and `platform`/`pkg` never import modules.

| Path | Role |
|------|------|
| `cmd/api` | Infra setup, catalog registration, lifecycle, graceful shutdown |
| `internal/bootstrap` | The single ordered module list + contract cross-wiring (shared with `test/arch`) |
| `cmd/migrate` | The only schema initializer/migrator |
| `cmd/seed` | Admin seeder (schema must exist) |
| `config` | One file per concern; `Config.Validate` runs section validators in a fixed order and reports the first error |
| `internal/app` | Fiber app, global middleware, module registry, provider hooks, `app.Infra` |
| `internal/modules/<m>` | `module.go` (Build + providers), `catalog/` (permissions, error codes, flags, locales), dto/handler/service/repository, `queries/` + generated `sqlc/`. Modules: identity, files, features, notifications, admin (contracts-only, no SQL), webhooks, apikeys |
| `internal/contracts/<provider>` | Stdlib-only contracts per providing module: identity (admin, principal reader, notification recipient), files (admin), notifications (notifier), webhooks (event/dispatcher) |
| `internal/platform` | authz, audit, events, features, i18n, jobs, requestctx, scheduler, session, tenant, tracing, versioning, middleware |
| `pkg` | apperror, response, database, cache, storage, email, webhook, ... |
| `migrations` | `000001_init_schema.*` baseline; later `<UTC ts>_<owner>_<purpose>.{up,down}.sql` |

**Request flow:** client → global middleware (request ID, i18n, tracing,
metrics, logger, audit fallback, timeout) → `/api/vN` guard → tenant
middleware → module route (auth limiter, authenticate, route limiter, authz,
idempotency, audit guard) → handler → service → repository → sqlc →
PostgreSQL. Diagram: `docs/architecture.md`.

**Design docs** (keep current when routes/contracts change):

| Doc | Scope |
|-----|-------|
| `docs/architecture.md` | Module map, ownership, request lifecycle, cross-module communication |
| `docs/data-model.md` | Core schema ER diagram |
| `docs/api-design.md` | Conventions, errors, pagination, idempotency, notifications, compatibility, update semantics |
| `docs/module-template.md` | Canonical module layout, scaffold, routes/permissions/tables/contracts |
| `docs/operations.md` | Health, metrics, logs, jobs, scheduler, shutdown, idempotency, migrations, incidents |
| `docs/tracing.md` | OpenTelemetry config, span privacy rules, collector, job trace links |
| `docs/feature-flags.md` | Catalog, precedence, cache, admin API, metrics |
| `docs/webhooks.md` | Outbound dispatch/signing/SSRF/retries, inbound receiver, secrets |

**Dependency rules** (enforced by `test/arch`; do not weaken it)
- A module must not import another module's internals; use
  `internal/contracts` or a narrow exported interface.
- `platform`/`pkg` must not import business modules.
- One module owns each table; cross-module reads use a contract/read model.
- `test/arch` builds every module from `internal/bootstrap` (v1 and v2 routes)
  and fails when: a `/api/v1`/`/api/v2` route lacks `authz.Require` (explicit
  public allowlist); a mutating route is neither Tier 1 (owning module records
  with `RecordInTx`) nor `audit.Guard`ed; `authz.Require`/`audit.Guard` sit
  after the final handler, or `Guard` before `Idempotency`; one module's group
  middleware covers another module's routes (the `/admin` overlap class); or a
  module directory is not registered in bootstrap.
- `TestCodeSQLOnlyTouchesOwnedTables` scans module `queries/*.sql` and inline
  SQL string literals in non-test Go under `internal/modules/**` and
  `internal/platform/**` (detected only with UPPERCASE keywords). Platform
  tables belong to `platform/<pkg>`; the one reviewed exception is
  `platform/features` reading `feature_flags`/`feature_flag_targets`. A CTE
  must not be named like another owner's table.
- Migrations: the baseline is spelled exactly `000001`; two spellings of one
  version are duplicates.

**Schema ownership**
- Only `cmd/migrate` applies DDL; API/seed assume the schema exists.
  `cmd/migrate` uses `config.LoadDatabase` (DB_* only; no JWT/admin/SMTP
  secrets).
- Fresh installs: one `migrations/000001_init_schema.up.sql`.
- New migrations: `make migrate-create module=<owner> name=<purpose>` →
  `<YYYYMMDDHHMMSS>_<owner>_<purpose>.{up,down}.sql`, always above the legacy
  sequential versions (an old deployment is at 3) and collision-free across
  branches. `test/arch` rejects duplicate versions, missing up/down pairs,
  non-timestamp versions, and unknown owners. golang-migrate never applies a
  version below the recorded one, so regenerate a branch migration older than
  the newest on main before merging.
- Legacy `000001`–`000003` databases cannot be upgraded from this repo's files
  (the arch test rejects restoring sequential versions): apply the reviewed
  upgrade DDL, `migrate force 1` only after the schema matches, then run
  `cmd/migrate` to verify (README "Upgrading an existing database"). Never
  point an old database at the new `000001`, rewrite `schema_migrations` to
  skip DDL, or edit a shipped migration.
- `jobs.trace_parent` is folded into the baseline (no deployed database); a
  database already at version 1 needs
  `ALTER TABLE jobs ADD COLUMN IF NOT EXISTS trace_parent TEXT` (see
  `docs/tracing.md`) before running this build.
- Tenant-owned tables (`users`, `files`, `notifications`,
  `notification_preferences`, `refresh_tokens`, `password_reset_tokens`,
  `email_verification_tokens`, `api_keys`, `jobs`, `audit_events`,
  `feature_flag_targets`, `webhook_*`) have
  `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)`. New ones must
  too, and lead tenant-scoped query indexes with `tenant_id`. `feature_flags`
  is process-wide (deliberate exception). Job claim/lease recovery/purge are
  global, so `jobs` has only the tenant-leading idempotency index.
- `cmd/migrate` verifies required tables, columns, unique indexes (every
  `ON CONFLICT` target), tenant-leading indexes, and enabled triggers
  (`trigger_protect_last_admin`, audit completion/no-delete/no-truncate); its
  test fails when that list drifts from the baseline. `audit_events` rejects
  `DELETE`/`TRUNCATE` at the database.
- `jobs.idempotency_key` is nullable with a partial unique
  `(tenant_id, idempotency_key)` index used by `EnqueueOnce`/`EnqueueOnceTx`.
- `scheduler_leases` (one leader row) and `scheduler_runs` (unique
  `(schedule, scheduled_for)` ledger) are platform coordination tables with no
  tenant column and no cross-module/jobs foreign keys; do not add them.
- The baseline seeds tenant id 1 (`default`); keep it as the compatibility
  tenant. The last-admin trigger is per tenant.

**Tenancy invariants**
- Identity, files, webhooks, apikeys, and notifications are tenant-isolated;
  every new query keeps a `tenant_id` predicate. Cross-tenant ids behave as
  not found (`404`), never forbidden. Deliberate exception: the identity
  `PurgeExpired*Tokens` maintenance queries delete expired tokens across all
  tenants (job `identity.tokens.purge_expired`).
- Startup checks tenant *existence* (`StartupTenants`, ignoring status)
  separately from request resolution: a suspended configured/default tenant
  must not crash `cmd/api`/`cmd/seed`; its requests return 404.
- `tenant.Middleware` resolves `X-Tenant-Slug` (empty → `TENANT_DEFAULT_SLUG`;
  production requires the header). A validated `?tenant=` fallback is accepted
  only on `/auth/google`, `/auth/reset-password`, `/auth/verify-email` (the
  header wins; reset/verify links carry it). The OAuth callback is exempt and
  uses the tenant in signed `state`.
- Tokens without a positive `tenant_id` claim are rejected fail-closed; rotate
  legacy tokens after upgrading. `tenant.EffectiveID(ctx)`'s tenant-1 fallback
  is for jobs/internal/tests only, never HTTP handling.
- RLS and per-tenant limits remain future work; do not claim
  whole-application isolation.

**Scope:** a boilerplate, not a finished multi-tenant SaaS platform.
Row-level security, per-tenant quotas, and promotion/rollout automation are
future work. Tracing (HTTP, sanitized pgx, jobs) is disabled by default
(`OTEL_ENABLED=false`). Kubernetes manifests and the GHCR release workflow are
a production foundation, not a complete CD platform. There is no shared
generated sqlc package, no cross-module sqlc import, and no legacy
handler/service/DTO/repository package.

## Error Handling

- Return `*apperror.AppError` from services/handlers via the typed catalog:
  `apperror.NewCode(identitycatalog.CodeUserEmailTaken, "public detail")`.
  Module codes live in the module's `catalog/` and register through
  `app.ErrorProvider`; only platform-wide codes (`permission.denied`,
  `feature.disabled`, `rate_limit.exceeded`, `api.version_unsupported`,
  `system.*`, and `user.last_admin` until the identity repository moves it)
  stay in `pkg/apperror`. An unregistered code renders as 500
  `system.internal` and logs, so handler/service unit tests call
  `<m>catalog.Register()`.
- Output is RFC 9457 `application/problem+json` (`type`, `title`, `status`,
  `code`, `detail`, `instance`, `requestId`, `timestamp`, optional
  `errors[]`). `detail` is public text only: never internal causes, SQL, or
  stack traces. Sentinel: `apperror.ErrNotFound`.
- `Deps.I18n` localizes titles and validation messages; localize a detail with
  `WithDetailKey("errors.<code>.detail", params)`. A nil bundle keeps English;
  `code`/`type`/`status`/`instance` never change.
- Permission failures go through `authz.Require` (`permission.denied`).

## Response & Pagination

- Use `pkg/response` (`Success`, `Created`, `NoContent`,
  `List(c, data, hasMore, nextCursor...)` → `{object,url,data,hasMore,nextCursor}`);
  never write raw JSON in handlers.
- Cursor pagination: `limit` (max 100) + `startingAfter`; repositories use
  `(created_at, id)` indexes; handlers return `nextCursor`
  (`pagination.NextCursor`) when `hasMore`. Offset helpers only for small
  admin/export tables.
- Existing `PUT`s are partial (identity, apikeys) or full replace (webhooks);
  new modules use `PATCH` for partial updates (`docs/api-design.md`).

## Key Patterns

- **i18n:** runs right after request ID; negotiates `Accept-Language`
  (q-values, truncation, wildcards), sets `Content-Language` and
  `Vary: Accept-Language`. Platform strings in `internal/platform/i18n/locales`;
  module strings in `catalog/locales/{en,vi}.json` (keys `<m>.*` or
  `errors.<code>.*`), merged via `app.LocaleSources`. Startup fails on key or
  `{placeholder}` parity mismatch, a missing locale file, a non-namespaced key,
  or a duplicate key.
- **API versions:** `/api/v1` always mounted; `app.V2Module`
  (`RegisterV2Routes`) opts into `/api/v2`. `Accept-Version: 1|v1|2|v2` must
  match the path, else 406 `api.version_unsupported`; unknown `/api/vN` → 406.
  Responses carry `API-Version`, `Vary: Accept-Version`, optional
  `Deprecation`/`Sunset`/`Link`. Keep Swagger's base path at `/api/v1`.
- **Auth/session:** JWT carries `token_version` and `tenant_id`;
  `session.Store` validates version, tenant, and tenant status with
  generation-keyed invalidation. `JWTAuth` rejects a non-positive tenant and an
  explicit header that conflicts with the claim, and clears `tenant_slug` when
  the claim overrides an unresolved default. `pkg/token` pins HS256 and
  requires `exp`. Refresh tokens are atomic single-use, tenant-scoped, and
  version-bound; logout deletes by possession; refresh with an inactive owner
  returns 401. Login lockout counts failures per (tenant, email, IP) and locks
  the account globally only after 5x that threshold. Lockout/rate-limit cache
  keys hash the normalized email (SHA-256, 128-bit hex:
  `<prefix><hash>:tenant=N[:ip=…]`), never store it raw. Banning sets
  `users.credentials_revoked_at` (`clock_timestamp()`); API keys created at or
  before it are rejected, so unban never revives them. API-key creation locks
  the owner row through `PrincipalLocker.LockPrincipalTx` (the only
  transactional apikeys→identity channel); rotate returns 409 when the owner
  is inactive or the key predates `credentials_revoked_at`.
  `(*AppError).Is(apperror.ErrNotFound)` matches any 404 `AppError`. Changing
  another user's email is refused for API-key callers and for admin targets.
  Google sign-in (`SignInWithGoogle` with a `Complete` hook) runs the
  tenant/email checks and session creation in the sign-in transaction before
  the Tier 1 `auth.google_login` row, so refused logins are never audited as
  success. The password-reset token insert re-checks the current email (an
  email change in between skips the send). The admin seed only creates a
  missing admin; an existing demoted/banned/deleted account is skipped, never
  re-promoted.
- **Authz:** module permissions live in `catalog/permissions.go`, registered
  via `app.PermissionProvider` (`authz.Grant{Permission, Roles}`; admins get
  everything). Only permissions shared by several modules (`admin:users:*`,
  `admin:files:*`, `admin:stats:read`) stay in `platform/authz`. Duplicate or
  conflicting registrations fail startup. Protect routes with
  `authz.Require(<m>catalog.PermissionX)` behind `deps.Authenticate()` (API key
  or JWT; API-key scopes are the subject's permissions). Only
  `POST /auth/logout-all` is JWT-only (`sessions:revoke`).
- **Files:** repositories always pass `tenant.EffectiveID(ctx)`; services also
  compare the row's `TenantID`/`UserID` before exposing it. Uploads reject an
  invalid tenant; blobs go under `<tenantID>/<userID>/<uuid>.<ext>`.
- **Jobs** (`internal/platform/jobs`, details in `docs/operations.md`):
  - At-least-once; handlers must be idempotent and honor context
    cancellation. Never use `async.Go` for durable work.
  - Reset/verification tokens commit with their `email.send` job in one
    transaction (`QueuedSender.SendTx`). `POST /auth/forgot-password` only
    enqueues an encrypted `password_reset.request` job (same response and work
    whether or not the account exists); the handler does the lookup and the
    token + email transaction. With transactional email wired (as in
    `identity.Build`), registration commits user, token, and `email.send` job
    together (fail-closed); otherwise the handler falls back to a post-commit
    email. Resend and email-change use a separate post-commit transaction.
  - Retries with backoff, dead-letter after `max_attempts`; unknown kinds retry
    until then. Claim errors back off up to 30s. Handler timeout 10m
    (`WithJobTimeout`, non-positive disables); a timeout is a normal failure.
  - Each row stores `lease_expires_at` = now() + the owning worker's lease
    (`WithLeaseDuration`, default 5m), set on claim and every heartbeat
    (default 30s); a `CHECK` requires it on `running` rows. Reclaim happens
    only when `lease_expires_at < now()`, so workers with different lease
    durations (dedicated slow-kind workers) are safe together.
    Bookkeeping writes are fenced by owner and attempts, so a reclaimed worker
    cannot finish the job, though it may still be running the old handler.
  - Shutdown: one 20s budget; HTTP, scheduler/worker, and async work drain in
    parallel within a 15s window, the rest is reserved for tracing flush and
    closing cache/DB. A request keeps working up to 3s after its handler
    (audit write 2s + idempotency store/release 1s =
    `config.PostHandlerReserve`/`app.PostHandlerBudget`), so
    `APP_REQUEST_TIMEOUT` is at most 12s (default 12). A handler that ignores
    cancellation keeps heartbeating and can overrun the drain window.
  - Terminal rows (`succeeded`/`dead`) older than 30 days are purged hourly
    from worker start (`WithRetention`, non-positive disables;
    `WithPurgeInterval`, non-positive ignored), 5s per pass, ≤1,000-row
    batches.
  - Metrics: `jobs_claimed_total{kind}`, `jobs_succeeded_total{kind}`,
    `jobs_retried_total{kind}`, `jobs_dead_total{kind}` (handler failures
    only, not lease-expiry dead letters), `jobs_claim_errors_total`,
    `jobs_purged_total`, `audit_write_failures_total`, and the
    `jobs_queue_lag_seconds{kind}` histogram (from the claim query, no extra
    query; a stalled queue shows as missing observations).
  - `jobs.WithKinds(...)`/`WithExcludeKinds(...)` filter claims for dedicated
    workers; lease reclaim stays global. A worker must register a handler for
    every kind it claims.
  - Email payloads are AES-256-GCM sealed with an HKDF-SHA256 key from
    `JWT_SECRET` (`jobs.PayloadCodec`) under kind `email.send.encrypted`; new
    workers register both kinds and still read legacy plaintext. Old workers
    leave encrypted jobs queued during a rolling deploy. Drain the queue
    before rotating `JWT_SECRET`.
- **Webhooks** (`docs/webhooks.md`): identity registration and file
  upload/delete/restore emit through `internal/contracts/webhooks.Dispatcher`
  (`app.Deps.Webhooks`). Dispatch writes a tenant-scoped delivery row plus a
  `webhook.delivery` job in one transaction, snapshotting URL/payload/secret
  ciphertext. The worker signs
  `<X-Webhook-Id>.<timestamp>.<X-Webhook-Event>.<raw-body>` with HMAC-SHA256,
  sends via `pkg/webhook.Client` (private/loopback/link-local blocked unless
  allowlisted, no redirects, no proxy, DNS pinned), records every attempt, and
  retries 408/425/429/5xx with backoff or `Retry-After`; other 3xx/4xx are
  dead. Receivers deduplicate on `X-Webhook-Delivery`. Secrets are sealed with
  `WEBHOOK_SECRET_ENCRYPTION_KEY` (JWT fallback locally; dedicated key in
  production) and returned only on create/rotate. `POST /webhooks/inbound/:id`
  uses the source tenant and HMAC and deduplicates `(source, X-Webhook-Id)`.
- **Events:** typed in-process bus; `Event` carries
  id/name/tenant/request/actor/timestamp/data, missing metadata resolved from
  context. `Subscribe` is synchronous local delivery; `SubscribeDurable`
  enqueues one job per event and handler with key
  `event:<name>:<id>:<handler>:<tenantId>`; `PublishTx` keeps it atomic with
  the business transaction. Panics are recovered per handler, errors joined;
  durable handlers must be idempotent.
- **Scheduler:** optional (`SCHEDULER_ENABLED`, default false), global. One
  process holds `scheduler_leases`; the `scheduler_runs` row and job enqueue
  share a transaction. Intervals align to epoch boundaries; cron is 5-field UTC
  plus `@hourly`/`@daily`/etc. `SCHEDULER_CATCHUP` is `none|latest|all`
  (bounded by `SCHEDULER_CATCHUP_LIMIT`); a lost heartbeat cancels the pass.
  Payload has `schedule`/`scheduledFor`/`data`; key `scheduler:<name>:<slot>`.
- **Module hooks:** `app.EventProvider`/`JobProvider`/`ScheduleProvider` run
  only when the dependency is non-nil, then durable event handlers register
  (webhooks registers `webhook.delivery`/`webhook.dispatch` this way). Catalog
  hooks (`PermissionProvider`, `ErrorProvider`, `FlagProvider`,
  `LocaleProvider`) are registered by `app.RegisterCatalogs`, which `cmd/api`
  calls right after `bootstrap.Build` and before FEATURE_FLAGS validation,
  `i18n.New`, and `app.New`; `app.Register` repeats it idempotently. The
  `email.send` handler stays in `cmd/api` (platform outbox kind used by
  several modules).
- **Audit:** privileged mutations are fail-closed. Tier 1 services call
  `audit.TxRecorder.RecordTx` in the mutation transaction (failure rolls back,
  `503 system.audit_unavailable`); Tier 2 routes use `audit.Guard` after
  idempotency (started intent row, completed as succeeded/failed/denied;
  webhook admin mutations and `POST /notifications/send` are Guarded).
  `audit.Middleware` is a fallback (only the webhook receiver and a user's own
  notification state rely on it) and skips routes that set `audit_guarded`;
  `status`/`path` are its reserved metadata keys. A zero `audit.Policy` mode is
  `ModeRequired` (best-effort must be explicit). Client-supplied text and
  metadata are sanitized before storage (invalid UTF-8 → U+FFFD in text
  columns, dropped in `jsonb`; NUL stripped). Use `audit.NewServiceEvent`;
  never put secrets or PII in metadata.
- **Tracing** (`docs/tracing.md`): `app.Deps.Tracing` is nil-safe; disabled is
  a no-op provider. Middleware goes after `RequestID`, before
  `Metrics`/`Logger`. Record only route templates and bounded `error.type`,
  never raw paths, payloads, SQL, headers, or error text. pgx is instrumented
  only under a recording parent. Jobs persist `trace_parent`; each attempt is a
  new-root `job.process` span linked to the enqueue span.
- **Notifications:** owns the in-app feed, per-type `in_app`/`email`
  preferences, and the email channel. Queries filter `tenant_id` and
  `user_id`; missing preferences mean both channels on. `notifications.Notifier`
  is the cross-module create API; identity implements
  `NotificationRecipientLookup`, so notifications never reads `users`. With
  email on, the row and `email.send` job commit together (queue failure rolls
  back); both channels off is a successful no-op. Writes use
  `Idempotency-Key`; admin sends need `notifications:send`.
- **Feature flags** (`docs/feature-flags.md`): flags declared in
  `catalog/flags.go` via `app.FlagProvider`; `config.Load` checks
  `FEATURE_FLAGS` syntax only and `cmd/api` rejects unknown keys after
  `app.RegisterCatalogs`. `deps.Flags` is never nil after `app.New`.
  Precedence: force-off > kill switch > user > tenant > database default >
  environment > compiled default. The `features` module scopes targets to
  `tenant.EffectiveID(ctx)` and invalidates the local snapshot after writes.
- **Transactions:** `database.TxManager.WithTx` scopes one module/aggregate;
  never span modules (use jobs/events).
- **Drivers:** storage `local|s3|seaweedfs` (S3 via aws-sdk-go-v2), cache
  `memory|redis`, email `console|smtp`, chosen by config factories.
- **Rate limits:** tiered, cache-backed; authenticated requests keyed per user,
  unauthenticated per IP.
- **Idempotency** (`docs/operations.md` flowchart): opt-in `Idempotency-Key`
  via `deps.Idempotency`, scoped by method/path/tenant ID+slug/caller (user, or
  IP for public routes). 2xx replays for 24h; non-multipart bodies ≤1 MiB are
  body-bound (different body → 409). The 2m lock is an atomic owner-token
  claim (`cache.Cache.SetNX` → `SET NX PX`, UUID token): concurrent duplicates
  get 409. On a lost reply the key is re-read (own token ⇒ held; other ⇒ 409;
  missing/read error ⇒ fail open), and compare-and-delete releases it even
  after failing open. The Redis client sets `ContextTimeoutEnabled`, so the
  250ms/500ms per-operation bounds (rate limit, idempotency) are real.
  Replays set `Idempotent-Replayed: true` and `idempotency_replayed=true` so
  audit skips them. Keep `Idempotency-Key` in `CORS_ALLOW_HEADERS`. Never add
  it to token-minting or secret-returning endpoints (API-key create/rotate,
  webhook create and `rotate-secret`) or GET/HEAD/OPTIONS.

## Adding a Module

Run `scripts/new-module.sh <name>` and follow `docs/module-template.md`. The
scaffold generates `module.go` (`Build(app.Infra)` + catalog providers + one
protected sample route), `module_test.go`, `catalog/` (`permissions.go`,
`errors.go`, `locales/en.json`, `locales/vi.json`),
handler/service/repository/dto, `queries/<name>.sql`, a `sqlc.yaml` block, and
`README.md`, then builds, vets, and tests the module.

1. Register it in `internal/bootstrap/modules.go` (`<name>.Build(infra)` in
   `List`, passing only contracts from earlier modules). This is the only
   central edit: catalogs self-register through the catalog providers, and
   jobs/events/schedules through `JobProvider`/`EventProvider`/
   `ScheduleProvider`. Add `RegisterV2Routes` only for a breaking v2 surface.
2. Keep handler/service/repository/DTO/catalog inside the module; protect
   every route with `authz.Require`.
3. Name tables `<name>`/`<name>_*` (or add them to the `test/arch` table
   ownership list); queries must not touch other modules' tables. Run
   `make sqlc-generate`.
4. Deployed DB: `make migrate-create module=<name> name=<purpose>`; otherwise
   update the single init schema.
5. Expose data to other modules only through `internal/contracts/<name>/`
   (stdlib-only).
6. Add unit + integration tests; `test/arch` must stay green.
7. Run `make swagger` and `make check`.

## Testing

- Standard library `testing` + `testify` where it improves assertions.
- Unit tests must not need Docker. Integration tests use `-tags=integration`
  with Testcontainers or `TEST_DATABASE_URL` (apply the schema with
  `cmd/migrate` first; platform integration tests use that database as-is).
- Run `go test -race ./...` before merging.
- Architecture tests live in `test/arch`.
