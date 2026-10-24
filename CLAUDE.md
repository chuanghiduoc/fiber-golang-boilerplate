# CLAUDE.md

Guidance for agents working in this repository.

## Build & Run

```bash
make build                 # server + migrate binaries
make migrate-run           # apply pending migrations via cmd/migrate
make run                   # migrate, then run the API locally
make seed                  # migrate (dependency) then seed the admin user
make test                  # unit tests
make test-integration      # integration tests (Docker/testcontainers or TEST_DATABASE_URL)
make check                 # vet + lint + unit tests
make lint                  # golangci-lint v2
make docker-run            # Compose: migrate job -> API
make docker-down
make sqlc-generate
make swagger
```

Production build: `CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o server ./cmd/api`.

CI builds both binaries and its `Migrations` job applies the baseline up/down
on a fresh PostgreSQL service, then runs `cmd/migrate` twice. Its `Manifests`
job runs actionlint, kustomize, and kubeconform. Reproduce the fresh-DB check
locally against a disposable database before merging.

Kubernetes manifests live in `deploy/k8s/`; follow `deploy/k8s/README.md`
(migration Job first, then the app kustomization). `.github/workflows/release.yml`
builds multi-arch GHCR images on `v*` tags.

## Architecture

**Modular monolith.** One deployable; modules own route tables; platform
packages own cross-cutting mechanics.

```
cmd/api/main.go             infra setup, catalog registration, lifecycle + graceful shutdown
internal/bootstrap/         the single ordered module list + contract cross-wiring (shared with test/arch)
cmd/migrate/main.go         the only schema initializer/migrator
cmd/seed/main.go            admin seeder (schema must exist)
internal/app/               Fiber app, global middleware, Module registry, provider hooks, app.Infra
internal/modules/           each module: module.go (Build + providers), catalog/ (permissions, error codes, flags, locales), dto/handler/service/repository, queries/sqlc. identity (full vertical slice + admin seed + queries/sqlc), files (dto/repository/service/handler + adapter + queries/sqlc), features (admin flag API + queries/sqlc), notifications (dto/repository/service/handler + queries/sqlc), admin (dto/service/handler, contracts-only), webhooks (dto/repository/service/handler + dispatcher/delivery worker + queries/sqlc)
internal/contracts/<provider>/ stdlib-only cross-module contracts per providing module: identity (admin, principal reader, notification recipient), files (admin), notifications (notifier), webhooks (event/dispatcher)
internal/platform/          authz, audit, events, features, i18n, jobs, requestctx, scheduler, session, tenant, tracing, versioning, and shared mechanics
pkg/                        apperror, response, database, cache, storage, email, ...
migrations/000001_init_schema.*   consolidated baseline; later migrations are <UTC ts>_<owner>_<purpose>.{up,down}.sql
deploy/k8s/                 Kubernetes manifests (see deploy/k8s/README.md)
```

**Request flow:** `Client → app middleware (request ID, i18n) → /api/vN guard → tenant middleware → module route → handler → service → repository → sqlc → PostgreSQL`.

**Design docs:** `docs/architecture.md` (module map, ownership, strangler
status), `docs/api-design.md` (API conventions, errors, pagination,
idempotency, notifications, compatibility), `docs/module-template.md` (canonical
module layout, scaffold, routes/permissions/tables/contracts),
`docs/operations.md` (health, metrics, logs, jobs, notification email,
idempotency, migrations, Kubernetes, incidents), `docs/tracing.md`
(OpenTelemetry configuration, span privacy rules, collector, job trace links),
`docs/feature-flags.md` (catalog, precedence, cache, admin API, metrics), and
`docs/webhooks.md` (outbound dispatch/signing/SSRF/retries, inbound receiver,
secrets). Keep them current when routes/contracts change.

**Dependency rules**
- A module must not import another module's internals. Use `internal/contracts`
  or a narrow exported interface.
- `platform`/`pkg` must not import business modules.
- One module owns each table; cross-module reads use a contract/read model,
  not another module's repository.
- Enforce with `test/arch/imports_test.go`; do not weaken it. `test/arch` also
  builds every module from `internal/bootstrap` and fails when a route under
  `/api/v1` lacks `authz.Require` (explicit public allowlist), when a
  mutating route is neither Tier 1 nor `audit.Guard`ed, when one module's
  group middleware covers another module's routes (the `/admin` overlap
  class), or when a module's `queries/*.sql` touch another module's tables.

**Schema ownership**
- Only `cmd/migrate` applies DDL. API/seed assume the schema exists.
- `cmd/migrate` uses `config.LoadDatabase` (DB_* only; no JWT/admin/SMTP
  secrets).
- Fresh installs: one `migrations/000001_init_schema.up.sql`.
- Existing databases: keep their `schema_migrations` history and add a
  migration above the current version with only the missing objects. New
  migrations use UTC timestamp versions (`make migrate-create module=<owner>
  name=<purpose>` -> `<YYYYMMDDHHMMSS>_<owner>_<purpose>.{up,down}.sql`), which
  are always above the legacy sequential versions (an old deployment is at 3)
  and do not collide across branches. `test/arch/migrations_test.go` rejects
  duplicate versions, missing up/down pairs, non-timestamp versions, and
  unknown owners. golang-migrate never applies a version below the recorded
  one, so regenerate a branch migration whose timestamp is older than the
  newest migration on main before merging. Never point an old database at
  the new `000001`, reset `schema_migrations`, or edit a shipped migration.
  The `jobs.trace_parent` column is folded into the baseline because this
  boilerplate has no deployed database; a database already at version 1 needs
  the `ALTER TABLE jobs ADD COLUMN IF NOT EXISTS trace_parent TEXT` upgrade in
  `docs/tracing.md` before running this build.
- Every tenant-owned table (`users`, `files`, `notifications`,
  `notification_preferences`, `refresh_tokens`, `password_reset_tokens`,
  `email_verification_tokens`, `jobs`, `audit_events`,
  `feature_flag_targets`, `webhook_*`) has
  `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)`.
  New tenant-owned tables must do the same and lead their query indexes with
  `tenant_id`. `feature_flags` is process-wide configuration and is the
  deliberate exception.
- `jobs.idempotency_key` is nullable with a partial unique
  `(tenant_id, idempotency_key)` index; `EnqueueOnce`/`EnqueueOnceTx` use it.
- `scheduler_leases` (one named leader row) and `scheduler_runs` (unique
  `(schedule, scheduled_for)` ledger) are platform coordination tables. They
  have no tenant column and no cross-module/jobs foreign keys; do not add them.
- The baseline seeds `tenants` id 1 (`default`). Keep id 1 as the compatibility
  tenant; the `users` last-admin trigger is scoped per tenant.
- Identity and files are tenant-isolated: identity queries scope users/admin
  lists/stats/refresh/reset/verification tokens; files scope every metadata
  read/write and admin operation. Public auth flows resolve the request tenant;
  Google OAuth binds the tenant into `state`; the session validator rejects
  tenant mismatches and suspended tenants.
- Startup checks tenant *existence* (`StartupTenants`, lookups ignoring status)
  separately from active request resolution. A suspended configured/default
  tenant must not crash `cmd/api`/`cmd/seed`; requests for it return 404.
- Reset/verification links carry `?tenant=<slug>`, and the tenant middleware
  accepts that validated query fallback only on the browser-facing auth paths
  (`/auth/google`, `/auth/reset-password`, `/auth/verify-email`); the header
  wins. The OAuth callback is exempt from middleware resolution and uses the
  tenant in signed `state`.
- Tokens without a `tenant_id` claim (or with a zero/negative claim) are
  rejected fail-closed; rotate legacy tokens after upgrading. Keep `tenant_id`
  predicates in every new query and index tenant-owned tables with `tenant_id`
  first. RLS and per-tenant limits remain future work; do not claim
  whole-application isolation.

**Scope:** this is a boilerplate, not a finished multi-tenant SaaS platform.
Row-level security, per-tenant limits/quotas, and environment
promotion/rollout automation remain future work. OpenTelemetry tracing exists
for HTTP, sanitized pgx queries, and durable jobs but is disabled by default
(`OTEL_ENABLED=false`); see `docs/tracing.md`.
`identity`, `files`, `webhooks`, `notifications`, and `admin` own all of
their DTOs, repositories, services, and handlers under
`internal/modules/<name>`; `admin` depends only on the stdlib-only
`internal/contracts` interfaces implemented by identity/files adapters and
never imports those modules. Notifications exposes `notifications.Notifier`
(`internal/contracts/notifications`) and consumes the identity contract
`NotificationRecipientLookup` (`internal/contracts/identity`), so
it never reads the users table. Each module with SQL owns its `queries/` and
generated `sqlc/` package (`admin` is contract-only); there is no shared
generated package and no cross-module sqlc imports, and no legacy
handler/service/DTO/repository packages remain.
Kubernetes manifests and a GHCR release workflow exist under `deploy/k8s/` and
`.github/workflows/release.yml`; they are a production foundation, not a
complete CD platform.

## Error Handling

- Return `*apperror.AppError` from services/handlers. Prefer the typed catalog:
  `apperror.NewCode(identitycatalog.CodeUserEmailTaken, "public detail")`.
  Module codes live in `internal/modules/<m>/catalog/errors.go` and are
  registered through `app.ErrorProvider`; only platform-wide codes
  (`permission.denied`, `feature.disabled`, `rate_limit.exceeded`,
  `api.version_unsupported`, `system.*`, and `user.last_admin` until the
  identity repository moves it) stay in `pkg/apperror`. An unregistered code
  renders as 500 `system.internal` and logs an error, so handler/service unit
  tests call `<m>catalog.Register()` (see `catalog_test.go` files).
- `pkg/apperror` renders RFC 9457 `application/problem+json` with `type`,
  `title`, `status`, `code`, `detail`, `instance`, `requestId`, `timestamp`,
  and optional `errors[]`.
- `detail` must be public text; never serialize internal causes, SQL, or stack
  traces. Sentinels: `apperror.ErrNotFound`.
- `Deps.I18n` localizes catalog titles and validation messages for
  `Accept-Language`. To localize a detail, attach
  `WithDetailKey("errors.<code>.detail", params)`. A nil bundle/localizer keeps
  the existing English output, and `code`/`type`/`status`/`instance` never
  change.
- Permission failures should use `authz.Require`; it returns
  `permission.denied`.

## Response & Pagination

Use `pkg/response`; never write raw JSON in handlers:
- `response.Success`, `response.Created`, `response.NoContent`
- `response.List(c, data, hasMore, nextCursor...)` →
  `{object,url,data,hasMore,nextCursor}`

Cursor pagination: `limit` (max 100) + `startingAfter`; repositories use
`(created_at, id)` indexes; handlers return `nextCursor` (built with
`pagination.NextCursor`) when `hasMore`. Offset helpers are for small
admin/export tables only. Existing `PUT`s are partial (identity, apikeys) or
full replace (webhooks); new modules use `PATCH` for partial updates (see
`docs/api-design.md` "Update semantics").

## Key Patterns

- **i18n:** `internal/platform/i18n` embeds `en`/`vi` catalogs and runs right
  after request ID. It negotiates q-values, truncation, and wildcards; sets
  `Content-Language` and `Vary: Accept-Language`; and localizes error titles
  and validation messages through `pkg/apperror`. Platform strings live in
  `internal/platform/i18n/locales`; module strings in
  `internal/modules/<m>/catalog/locales/{en,vi}.json` (keys `<m>.*` or
  `errors.<code>.*`), merged via `app.LocaleSources` into `i18n.New`. Keep
  keys and `{placeholders}` in parity; startup fails on parity mismatch, a
  missing locale file, a non-namespaced key, or a key defined twice.
- **API versions:** `/api/v1` is always mounted. Implement `app.V2Module`
  (`RegisterV2Routes`) to opt into `/api/v2`. `Accept-Version: 1|v1|2|v2` must
  match the path, otherwise 406 `api.version_unsupported`. Version middleware
  sets `API-Version`, `Vary: Accept-Version`, and optional
  `Deprecation`/`Sunset`/`Link`; `versioning.Guard` returns 406 for unknown
  `/api/vN`. Keep Swagger's base path at `/api/v1`.
- **Auth/session:** JWT carries `token_version` and `tenant_id`;
  `session.Store` validates the version, tenant, and tenant status with
  generation-keyed invalidation. `tenant.Middleware` resolves `X-Tenant-Slug`
  (empty -> `TENANT_DEFAULT_SLUG`; production requires the header), accepts a
  validated `?tenant=` fallback on browser-facing auth paths, and skips the
  OAuth callback so the signed `state` tenant wins. `JWTAuth` rejects tokens
  without a positive tenant and an explicit header that conflicts with the
  claim, and clears `tenant_slug` when the claim overrides an unresolved
  default. Identity repositories scope every query with
  `tenant.EffectiveID(ctx)`; the tenant-1 fallback is for jobs/internal/tests
  only, not HTTP request handling. Refresh tokens are atomic single-use,
  tenant-scoped, and version-bound; logout deletes by token possession.
  `pkg/token` pins HS256 and requires `exp`.
- **Files/tenant isolation:** repository methods always add
  `tenant.EffectiveID(ctx)` to the SQL parameters; service methods also compare
  the returned row's `TenantID`/`UserID` before exposing it. Uploads reject an
  invalid tenant and store blobs under `<tenantID>/<userID>/<uuid>.<ext>`.
  Cross-tenant ids must behave as not found (`404`), never forbidden, so file
  existence is not disclosed.
- **Jobs:** `internal/platform/jobs` is the durable queue; delivery is
  at-least-once, so handlers must be idempotent. Services enqueue email/webhook
  work; password-reset and verification tokens are committed with their
  `email.send` job in one transaction (`QueuedSender.SendTx`), so an enqueue
  failure rolls the token back. When transactional email provisioning is wired
  (as in `identity.Build`), registration is atomic too: `UserService.Register`
  creates the user, verification token, and `email.send` job in one transaction,
  so a failed outbox write rolls the account back (fail-closed); without that
  wiring the handler falls back to a post-commit email. Resend and email-change
  flows keep their separate post-commit token/email transaction. A worker
  retries with backoff and dead-letters after `max_attempts`.
  Claim errors are retried in-process with backoff capped at 30s. Handlers have
  a 10m default timeout (`WithJobTimeout`, non-positive disables it), and
  unknown kinds retry until `max_attempts` before dead-lettering. Heartbeats
  renew a 5m lease every 30s; expired leases are reclaimed. Handlers must honor
  context cancellation: the worker cannot preempt a handler that ignores it,
  heartbeats continue until it returns, and a non-cooperative handler can
  exceed the shutdown drain window (the app waits up to 10s). Bookkeeping writes
  are fenced by `attempts`, so a reclaimed worker cannot finish the job, though
  it may still be executing the old handler. Do not use `async.Go` for durable
  work. Terminal rows (`succeeded`/`dead`) are purged hourly once older than
  30 days (`WithRetention`; non-positive disables) at `WithPurgeInterval`
  (non-positive values are ignored), starting immediately when a worker boots.
  Only terminal rows are eligible; each pass is bounded to 5s and deletes in
  batches of at most 1,000 rows. Prometheus counters cover job lifecycle
  (`jobs_claimed_total{kind}`, `jobs_succeeded_total{kind}`,
  `jobs_retried_total{kind}`, `jobs_dead_total{kind}`), claim failures
  (`jobs_claim_errors_total`), purges (`jobs_purged_total`), and audit write
  failures (`audit_write_failures_total`). `jobs_dead_total` covers handler
  failures that exhaust attempts, not lease-expiry dead letters.
  `jobs_queue_lag_seconds{kind}` (histogram) is observed per claimed job from
  the claim query itself (database `now() - run_at`), so it adds no query; a
  fully stalled queue shows as missing observations. `jobs.WithKinds(...)` /
  `jobs.WithExcludeKinds(...)` filter the claim query so a deployment can run
  a dedicated worker (own concurrency/pod) for slow kinds; the expired-lease
  reclaim stays global. A worker must register a handler for every kind it
  claims. Email job
  payloads are sealed with AES-256-GCM using an HKDF-SHA256 key derived from
  `JWT_SECRET` (`jobs.PayloadCodec`); `EmailHandler` also accepts legacy
  plaintext JSON, which remains readable at rest until delivered or cleared.
  External tools see the envelope in `jobs.payload`. Encrypted jobs use the
  distinct kind `email.send.encrypted`; new workers register both kinds. During
  a rolling deploy old workers leave encrypted jobs queued (unknown-kind retry)
  instead of misreading them, so deploy the new worker to drain the queue.
  Rotating `JWT_SECRET` invalidates queued encrypted jobs, so drain the queue
  first.
- **Webhooks:** identity registration and file upload/delete emit through
  `internal/contracts/webhooks.Dispatcher` (`app.Deps.Webhooks`), so producers
  never import the module. Dispatch inserts
  a tenant-scoped `webhook_deliveries` row plus a `webhook.delivery` job in one
  transaction and snapshots URL/payload/secret ciphertext. The worker signs
  `<timestamp>.<raw-body>` with HMAC-SHA256, sends through `pkg/webhook.Client`
  (private/loopback/link-local blocked unless allowlisted, no redirects, no
  proxy, DNS pinned to vetted IPs), records every attempt, and retries 408/425/
  429/5xx with exponential backoff or `Retry-After`; other 3xx/4xx are dead.
  Receivers deduplicate on `X-Webhook-Delivery`. Secrets are AES-256-GCM sealed
  with `WEBHOOK_SECRET_ENCRYPTION_KEY` (JWT fallback locally; dedicated key
  required in production) and returned only on create/rotate. The public
  `POST /webhooks/inbound/:id` receiver uses the source tenant and HMAC, and
  deduplicates `(source, X-Webhook-Id)`. See `docs/webhooks.md`.
- **Events:** `internal/platform/events` is a typed in-process bus. `Event`
  carries id/name/tenant/request/actor/timestamp/data; missing tenant, request,
  and actor metadata is resolved from the context. `Subscribe` is synchronous
  local delivery; `SubscribeDurable` enqueues one job per event and handler
  with key `event:<id>:<handler>:<tenantId>`; `PublishTx` keeps the job and the
  business transaction atomic. Panics are recovered per handler and errors are
  combined with `errors.Join`. Durable handlers are at-least-once, so they must
  be idempotent. `RegisterJobs` wires subscriptions into the worker.
- **Scheduler:** `internal/platform/scheduler` is optional
  (`SCHEDULER_ENABLED`, default false) and global, not tenant-scoped. One
  process holds `scheduler_leases`; a unique `scheduler_runs` row plus the job
  enqueue share a transaction. Interval schedules align to epoch boundaries;
  cron is 5-field UTC with lists/ranges/steps and `@hourly`/`@daily`/etc.
  `SCHEDULER_CATCHUP` is `none|latest|all` (bounded by
  `SCHEDULER_CATCHUP_LIMIT`); a lost heartbeat cancels the current pass. Job
  payloads include `schedule`/`scheduledFor`/`data` and keys are
  `scheduler:<name>:<slot>`.
- **Optional module hooks:** implement `app.EventProvider`,
  `app.JobProvider`, or `app.ScheduleProvider` to register events, jobs, or
  schedules (the webhooks module registers `webhook.delivery`/`webhook.dispatch`
  this way). `app.Register` invokes them only when the corresponding dependency
  is non-nil, then registers durable event job handlers. Catalog hooks
  (`PermissionProvider`, `ErrorProvider`, `FlagProvider`, `LocaleProvider`)
  are registered by `app.RegisterCatalogs`, which `cmd/api` calls right after
  `bootstrap.Build` and before FEATURE_FLAGS validation, `i18n.New`, and
  `app.New`; `app.Register` repeats it idempotently. The `email.send` handler
  stays in `cmd/api`: it is a platform outbox kind produced by several
  modules.
- **Audit:** privileged mutations are fail-closed. Tier 1 services call
  `audit.TxRecorder.RecordTx` in the mutation transaction (failure rolls back
  and returns `503 system.audit_unavailable`); Tier 2 routes use
  `audit.Guard` after idempotency to write a started intent row and complete it
  as succeeded/failed/denied (webhook admin mutations and
  `POST /notifications/send` are Guarded). `platform/audit.Middleware` remains
  a fallback (only the webhook receiver and a user's own notification state
  rely on it) and skips routes that set `audit_guarded`. Use `audit.NewServiceEvent` for
  domain events; never place secrets or PII in metadata.
- **Tracing:** `internal/platform/tracing` owns the provider; `app.Deps.Tracing`
  is nil-safe and disabled means a no-op provider. Install the middleware after
  `RequestID` and before `Metrics`/`Logger`; record only route templates and
  bounded `error.type`, never raw paths, payloads, SQL, headers, or error text.
  `pkg/database.WithTracerProvider` instruments pgx only under a recording
  parent. Jobs persist `trace_parent` on enqueue and start each attempt as a
  new-root `job.process` span linked to the enqueue span. See
  `docs/tracing.md` before adding attributes.
- **Notifications:** `internal/modules/notifications` owns the in-app feed,
  per-type `in_app`/`email` preferences, and the email channel. Read/list/
  unread/read-all and preference queries filter both `tenant_id` and `user_id`;
  cross-tenant or other-user ids return `404`. Missing preferences mean both
  channels are enabled. `notifications.Notifier` is the cross-module create API;
  `identity.NotificationRecipientLookup` lets identity supply the email
  address without a module import. When email is enabled, the in-app row and
  `email.send` job commit in one transaction (a queue failure rolls the row
  back); the job inherits `jobs.tenant_id`. A suppressed notification (both
  channels disabled) is a successful no-op. Routes are under
  `/notifications` and `/notification-preferences`; writes use
  `Idempotency-Key`, and admin sends require `notifications:send`.
- **Authz:** module permissions live in `internal/modules/<m>/catalog/permissions.go`
  and are registered through `app.PermissionProvider` (`authz.Grant{Permission,
  Roles}`; admins get every permission). Only permissions checked by more than
  one module stay in `platform/authz` (the `admin:users:*`, `admin:files:*`,
  `admin:stats:read` set used by the admin routes and the identity/files
  services). Duplicate or conflicting registrations fail startup. Protect
  routes with `authz.Require(<m>catalog.PermissionX)` behind
  `deps.Authenticate()` (API key or JWT; API-key scopes are the subject's
  permissions). Only `POST /auth/logout-all` is JWT-only (with
  `sessions:revoke`).
- **Feature flags:** the evaluator lives in `platform/features`; flags are
  declared in `internal/modules/<m>/catalog/flags.go` and registered through
  `app.FlagProvider`. `config.Load` checks `FEATURE_FLAGS` syntax only;
  `cmd/api` rejects unknown keys after `app.RegisterCatalogs`. Modules read `deps.Flags` (never nil after `app.New`).
  Precedence is force-off > kill switch > user > tenant > database default >
  environment > compiled default. The `features` module owns the admin API,
  scopes targets to `tenant.EffectiveID(ctx)`, and invalidates the local
  snapshot after writes. See `docs/feature-flags.md`.
- **Transactions:** `database.TxManager.WithTx` scopes a transaction to one
  module/aggregate. Do not span modules; use jobs/events.
- **Drivers:** storage (`local|s3|seaweedfs`, S3 via aws-sdk-go-v2), cache (`memory|redis`), email
  (`console|smtp`) are selected by config factories.
- **Rate limits:** tiered, cache-backed in `internal/app`; authenticated
  requests are keyed per user, unauthenticated per IP.
- **Idempotency:** opt-in `Idempotency-Key` on mutating routes via
  `deps.Idempotency`; scoped by method/path/tenant ID+slug/caller (user, or IP
  for public routes). 2xx
  responses replay for 24h; non-multipart bodies up to 1 MiB are body-bound, so
  reuse with a different body gets 409. An owner-token lock is held for 2m
  (compare-and-delete when the cache supports it), concurrent duplicates get
  409, and cache errors fail open. Replays set `Idempotent-Replayed: true` and
  `idempotency_replayed=true` so audit skips them. Keep `Idempotency-Key` in
  `CORS_ALLOW_HEADERS`; never add the middleware to token-minting or
  secret-returning endpoints (API-key create/rotate, webhook create and
  `rotate-secret`) or GET/HEAD/OPTIONS.

## Adding a Module

Run `scripts/new-module.sh <name>` and follow `docs/module-template.md`. The
scaffold generates `module.go` (`Build(app.Infra)` constructor + catalog
providers + one protected sample route), `module_test.go`, `catalog/`
(`permissions.go`, `errors.go`, `locales/en.json`, `locales/vi.json`),
handler/service/repository/dto, `queries/<name>.sql`, a `sqlc.yaml` block,
and `README.md`, then builds, vets, and tests the module.

1. Register it in `internal/bootstrap/modules.go` (`<name>.Build(infra)` in
   `List`, passing only contracts from earlier modules). This is the only
   central edit: permissions, error codes, flags, and strings self-register
   through `app.PermissionProvider`/`ErrorProvider`/`FlagProvider`/
   `LocaleProvider`; jobs/events/schedules through `JobProvider`/
   `EventProvider`/`ScheduleProvider`. Add `RegisterV2Routes`
   (`app.V2Module`) only for a breaking v2 surface.
2. Keep handler/service/repository/DTO/catalog inside the module; protect
   every route with `authz.Require` (`test/arch` enforces it).
3. Name tables `<name>` / `<name>_*` (or add them to
   `test/arch/table_ownership_test.go`); queries must not touch other
   modules' tables. Run `make sqlc-generate`.
4. Add a migration with `make migrate-create module=<name> name=<purpose>`
   for a deployed DB; otherwise update the single init schema.
5. Expose data to other modules only through
   `internal/contracts/<name>/` (stdlib-only).
6. Add unit + integration tests; `test/arch` must stay green.
7. Run `make swagger` and `make check`.

## Testing

- Standard library `testing` + `testify` where it improves assertions.
- Unit tests must not need Docker. Integration tests use `-tags=integration`
  and either Testcontainers or `TEST_DATABASE_URL`.
- Run `go test -race ./...` before merging.
- Architecture tests live in `test/arch`.
```
