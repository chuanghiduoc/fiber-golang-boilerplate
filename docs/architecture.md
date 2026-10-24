# Architecture

This repository is a **modular monolith**: one deployable, module-owned HTTP
surfaces, and platform packages for cross-cutting mechanics. The goal is to
support many business modules without microservice overhead.

## Runtime shape

```
cmd/api        infra setup, catalog registration, worker lifecycle, graceful shutdown
cmd/migrate    the only DDL entrypoint
cmd/seed       standalone admin seeder (schema must already exist)

internal/app   Fiber, middleware, infra routes, module registry + provider hooks, app.Infra
internal/bootstrap  the single ordered module list + contract cross-wiring (cmd/api, test/arch)
internal/modules
  identity     auth, users, sessions, tokens, admin seed
               (dto/repository/service/handler + adapter + queries + generated sqlc)
  files        upload, metadata, download, soft delete, restore
               (dto/repository/service/handler + adapter + queries + generated sqlc)
  features     admin feature-flag catalogue surface
  notifications in-app feed, channel preferences, durable email channel
               (dto/repository/service/handler + queries + generated sqlc)
  admin        back-office facade (dto/service/handler, contracts-only)
  webhooks     outbound endpoints/deliveries, inbound sources/events,
               public HMAC receiver, dispatcher + delivery worker
               (dto/repository/service/handler + queries + generated sqlc)
  apikeys      tenant-scoped API-key lifecycle (dto/repository/service/handler + adapter)
internal/contracts/<provider>  stdlib-only cross-module contracts grouped by the
                    providing module: identity (admin, principal reader,
                    notification recipient), files (admin), notifications
                    (notifier), webhooks (event/dispatcher)
internal/platform
  apikey       key generation/hash, verifier, rotation grace, ApiKey middleware
  authz        subject/policies, permission catalog, ABAC + route middleware
  audit        audit recorder + mutating-request middleware
  events       typed in-process bus + durable job-backed subscriptions
  features     compile-time flag catalog, evaluator, snapshot cache, DB store
  i18n         embedded en/vi catalogs, Accept-Language negotiation, localization
  jobs         durable PostgreSQL queue, worker, leases, retention
  requestctx   request id + actor propagation through context
  scheduler    PostgreSQL-leader interval/cron scheduler + unique run ledger
  session      token-version/session cache
  tenant       request tenant resolver + locals helpers (X-Tenant-Slug)
  versioning   V1/V2 policy, Accept-Version assertions, deprecation headers
internal/platform/middleware   HTTP middleware (JWT, rate limit, logger, idempotency, ...)
pkg            reusable infrastructure (errors, response, database, cache, webhook, ...)
```

Request flow:

```
client
  -> app global middleware (CORS, security, request ID, i18n, tracing, metrics,
     logger, recovery, audit, timeout, idempotency where wired)
  -> /api version guard (unknown /api/vN -> 406)
  -> /api/vN version middleware (Accept-Version, API-Version, deprecation)
  -> tenant middleware (X-Tenant-Slug, browser auth query, or default)
  -> module route (JWT|ApiKey -> rate limit -> authz -> idempotency)
  -> handler -> service -> repository -> sqlc -> PostgreSQL
```

`i18n.Middleware` runs immediately after request ID so every response,
including errors, carries the negotiated locale. `pkg/apperror` uses the
request locale for catalog titles and validation messages. `versioning.Guard`
runs on `/api` before the version groups; `versioning.Middleware` is mounted on
each group. The v2 group exists only when a module implements `app.V2Module`.

The webhooks public receiver is the deliberate exception: it mounts auth per
route instead of on the `/webhooks` group, so `POST /webhooks/inbound/:id`
bypasses JWT and tenant-header selection. The source row supplies the tenant and
the source HMAC authenticates the caller.

Notification creation follows the same flow for HTTP reads and preferences.
Other modules call `notifications.Notifier` (`internal/contracts/notifications`),
and the notifications service resolves the recipient's email through
`identity.NotificationRecipientLookup` (`internal/contracts/identity`)
(implemented by identity). The service never imports identity or reads the
`users` table.

`tenant.Middleware` runs on the `/api/v1` and `/api/v2` groups, before module
auth. It validates `[a-z0-9-]{1,63}`, resolves the slug (header; validated
`?tenant=` fallback on `/auth/google`, `/auth/reset-password`, and
`/auth/verify-email`; otherwise `TENANT_DEFAULT_SLUG`), and stores
`tenant_id`/`tenant_slug` in Fiber locals and the request context. The Google
OAuth callback is exempt so `TENANT_REQUIRE_HEADER=true` cannot reject it
before the handler validates the signed `state`; the handler installs the
state tenant itself. `JWTAuth` treats the JWT `tenant_id` claim as
authoritative, rejects a non-positive claim, and rejects an explicit header
that resolves to a different tenant with 403. It clears `tenant_slug` when a
claim overrides a header-less default resolution rather than leaving a
misleading slug. `apikey.Authenticate` wraps the JWT handler:
`Authorization: ApiKey ...` verifies the credential, re-reads the owner
principal, and installs the same locals plus an `authz.Subject` limited to the
key's scopes. Infrastructure probes (`/healthz`, `/readyz`, `/metrics`) stay
outside tenant resolution so they never depend on the `tenants` table.

## Multi-tenant model

- One user belongs to exactly one tenant. `tenants.id` 1 with slug `default`
  is seeded by the baseline and preserves backward compatibility for header-less
  local/test requests and rows written before multi-tenancy. Production sets
  `TENANT_REQUIRE_HEADER=true`, so every HTTP write selects a tenant explicitly.
  Startup checks tenant existence regardless of status; a suspended default
  tenant starts normally and its requests return 404.
- `tenant_id` is present on `users`, `files`, `notifications`,
  `notification_preferences`, `feature_flag_targets`, `api_keys`, `webhook_*`, and all
  identity/platform token, job, and audit tables, with
  `DEFAULT 1 REFERENCES tenants(id)`.
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
- Webhooks are tenant-isolated at the SQL/index layer: endpoints, versioned
  secrets, deliveries, attempts, inbound sources, and inbound events all lead
  with `tenant_id`. The only unscoped lookup is the public receiver's source
  lookup by ID; it authenticates with the source HMAC and installs the source
  tenant before writing or auditing.
- Notifications are tenant- and user-scoped. Every repository query filters
  `(tenant_id, user_id)`, so a notification id from another tenant or another
  user in the same tenant behaves as not found. Preferences use the same scope
  and a unique `(tenant_id, user_id, type, channel)` index; a missing row means
  both channels are enabled.
- Durable jobs, events, and audit records propagate the resolved tenant:
  enqueue writes `jobs.tenant_id` from the context, the worker restores it with
  `tenant.WithID` before calling a handler, event metadata carries the
  publishing tenant/request/actor, and the audit middleware records the request
  tenant. Job retention and job/event metrics stay global. Notification email
  jobs are enqueued inside the notification transaction, so they inherit the
  notification tenant.
- The scheduler is a single global leader, not a tenant-scoped worker. Each
  schedule chooses a tenant in its spec; the enqueued job stores that tenant and
  the worker restores it like any other job. `scheduler_leases` and
  `scheduler_runs` deliberately have no tenant column or foreign keys.
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
- Feature-flag targets are tenant-owned (`feature_flag_targets.tenant_id` and
  nullable `user_id`). The repository takes the tenant from
  `tenant.EffectiveID(ctx)`; a composite foreign key prevents a target from
  pointing at a user in another tenant. Flag definitions/defaults and kill
  switches are process-wide; per-tenant rollout uses targets. See
  [Feature Flags](feature-flags.md).


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
  request ends. `EnqueueOnce`/`EnqueueOnceTx` add a deterministic
  `(tenant_id, idempotency_key)` unique constraint for producer retries and
  event fan-out.
- `events.PublishTx` enqueues one durable job per subscription inside the
  caller's transaction. Local event handlers run synchronously before commit,
  so keep them free of irreversible external side effects.
- The scheduler writes `scheduler_runs` and its job in one transaction; the
  unique `(schedule, scheduled_for)` ledger is the idempotency boundary across
  leader changes. The lease row is the only cross-replica coordination point.
- Email delivery and durable events are at-least-once. Handlers must be
  idempotent. Terminal jobs clear their payload and are purged after
  retention; retention is global and ignores tenant boundaries.
- Email payloads are encrypted at rest with AES-256-GCM; keys are derived from
  `JWT_SECRET` via HKDF. Rotate the secret only after draining the queue.
- Outbound webhooks reuse the jobs outbox: a delivery row and its first
  `webhook.delivery` job commit in one transaction. Identity registration,
  file upload/delete (user and admin), and file restore emit
  `user.registered`, `file.uploaded`, `file.deleted`, and `file.restored`
  through the webhooks outbox (`DispatchTx`) inside the mutation's
  transaction, so an enqueue failure rolls the mutation back (fail-closed);
  only an unwired deployment falls back to a logged post-commit dispatch. Each delivery
  snapshots the
  URL, payload, event timestamp, max attempts, and sealed secret so later
  endpoint edits/rotation cannot change an in-flight request. Webhook-level
  retries schedule new jobs (honoring `Retry-After`), and every attempt is kept
  in `webhook_delivery_attempts`. Receivers must deduplicate on
  `X-Webhook-Delivery`.
- The public inbound receiver resolves its tenant from the source row and
  verifies HMAC over `<X-Webhook-Id>.<timestamp>.<X-Webhook-Event>.<raw-body>`; deduplication is unique per
  `(tenant, source, external_event_id)`. See [Webhooks](webhooks.md).
- Notification creation is atomic when the email channel is enabled: the
  in-app row and its email job commit or roll back together. A suppressed
  notification (both channels disabled) writes nothing. If an email sender is
  wired without transactional support, the service falls back to a
  post-insert send; production wiring always uses the queued transactional
  sender.

## Schema ownership

- Only `cmd/migrate` applies DDL. The API and seed assume the schema exists.
- Fresh installs use one baseline: `migrations/000001_init_schema.{up,down}.sql`.
- Once a database is deployed, add timestamped migrations
  (`make migrate-create module=<owner> name=<purpose>`); never edit a shipped
  migration. Legacy databases must keep their recorded history (see README).
- Tenant-owned tables carry `tenant_id` with a default of 1 and a foreign key to
  `tenants`; tenant-leading indexes back scoped queries. The baseline seeds the
  `default` tenant as row 1, so existing rows and header-less requests remain on
  the compatibility tenant. `feature_flags` is global configuration;
  `feature_flag_targets` is tenant-owned and uses a `(tenant_id, user_id)`
  composite foreign key to keep user targets inside the tenant.
- Platform coordination tables are intentionally tenant-independent:
  `scheduler_leases` holds one named lease, and `scheduler_runs` is a unique
  run ledger with no foreign keys to jobs or tenants. `jobs.idempotency_key`
  is nullable with a partial unique `(tenant_id, idempotency_key)` index.
- `api_keys` stores a globally unique SHA-256 hash and scopes as a JSONB array,
  with hash, tenant/user, and cursor indexes; the only global lookup is by hash,
  before a request tenant can be trusted.
- File blobs are external to PostgreSQL but follow the row's tenant: new keys
  are `<tenantID>/<userID>/<uuid>.<ext>`, while `files.storage_path` is the
  authoritative key for downloads. Moving a blob without updating that column
  (or vice versa) breaks the file.
- `notifications` and `notification_preferences` belong to the notifications
  module. The feed index leads with `(tenant_id, user_id, created_at DESC,
  id DESC)` and has a partial unread variant; the unique preference index makes
  the upsert idempotent.
- `cmd/migrate` verifies required tenant tables/columns/indexes (including
  notifications and the unique preference index) and that the default tenant
  exists with id 1.

## Cross-cutting conventions

- **Authorization:** module permissions live in each module's
  `catalog/permissions.go` and are registered via `app.PermissionProvider`;
  only cross-module shared permissions stay in `internal/platform/authz`.
  Every route uses `authz.Require` (enforced by `test/arch`). Roles are an implementation detail of the permission
  matrix. Notification routes use `notifications:read`/`notifications:write`;
  sending to another user requires the admin-only `notifications:send`. Current
  legacy exceptions (public auth routes, some identity handler checks) are listed in
  the [Module Template](module-template.md#legacy-exceptions-do-not-copy).
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
  24h replay, body-bound for non-multipart requests. Secret-returning creates
  are excluded so plaintext secrets never enter the shared response cache.
- **Feature flags:** module-declared catalog (`catalog/flags.go`,
  `app.FlagProvider`) evaluated by `platform/features`; precedence
  force-off > kill switch > user > tenant > database default > environment >
  compiled default. Admin writes go through the `features` module and invalidate
  the local snapshot. See [Feature Flags](feature-flags.md).
- **Observability:** request IDs, structured `slog`, Prometheus HTTP/job/event/
  scheduler/audit metrics, readiness checks for DB/cache. Event and scheduler
  labels stay bounded (`event`, `schedule`, `outcome`), never tenant IDs.

## Strangler status

The migration is complete. `identity`, `files`, `notifications`, `admin`, and
`webhooks` own their DTOs, repositories, services, and HTTP handlers under
`internal/modules/<name>`; there are no shared legacy
handler/service/DTO/repository packages or compatibility aliases.

The `admin` module depends on `internal/contracts/identity.IdentityAdmin` and
`internal/contracts/files.FilesAdmin`; identity and files implement those contracts
with adapters in their own `adapter` packages. Admin never imports another
module, its repository, generated SQL, storage, database transactions, or
session store. Identity and files each own their half of the dashboard stats
(`AdminUserStats` / `AdminFileStats`), so no cross-module SQL remains.

`identity`, `files`, `webhooks`, and `notifications` own module-scoped
`queries/` and generated `sqlc/` packages; `sqlc.yaml` has one block per module.
There is no shared generated package, and no module imports another module's
sqlc types. Notifications exposes `notifications.Notifier` to other modules and
consumes `identity.NotificationRecipientLookup`, implemented by the identity
adapter; neither direction imports module internals. New code should start in
`internal/modules/<name>/` and use contracts for cross-module reads; webhook
producers use `internal/contracts/webhooks`.

## Extension checklist

Use [`scripts/new-module.sh`](../scripts/new-module.sh) and the canonical
[Module Template](module-template.md) for the full layout, skeleton, and
ownership rules. Short form:

1. `scripts/new-module.sh <name>`: module.go (`Build` + providers), catalog
   (permissions, error codes, en/vi strings), layers, `queries/`, sqlc block,
   module test, README.
2. Add the module to `internal/bootstrap/modules.go` (the only central edit).
3. Add tables: update the baseline for fresh installs, or
   `make migrate-create module=<name> name=<purpose>` for deployed DBs.
4. Add handler/service/repository/DTO and protected routes in the module.
5. Implement the optional `app.EventProvider`, `app.JobProvider`,
   `app.ScheduleProvider`, or `app.FlagProvider` hooks for durable events,
   jobs, schedules, or flags instead of wiring them imperatively.
6. Add unit + integration tests; run `make check`, `go test -race ./...`, and
   `make swagger`.
7. For durable work, enqueue a job transactionally; never use `async.Go` for
   work that must survive a restart.

## Module registration and catalogs

Startup is one explicit path in `cmd/api/main.go`:

```
config.Load (FEATURE_FLAGS syntax only)
  -> infra: tracing, pool, storage, cache, flag evaluator, email, tx, audit,
     job queue/worker, event bus, queued email sender  => app.Infra
  -> bootstrap.Build(infra)         every module's Build, contract cross-wiring
  -> app.RegisterCatalogs(list...)  permissions, error codes, flags (owner = Name())
  -> cfg.FeatureFlags.ValidateKnownKeys()   unknown FEATURE_FLAGS keys fail here
  -> i18n.New(locale, app.LocaleSources(list...)...)   merge + parity check
  -> Modules.Startup(ctx)           tenant existence + admin seed
  -> app.New(cfg, deps) ; app.Register(list...)   routes, jobs, events, schedules
  -> email.send handlers, scheduler, worker, Modules.Background, HTTP listen
```

- Catalog registries (`authz`, `apperror`, `features`) are process-wide,
  copy-on-write snapshots behind an `atomic.Pointer`, so the per-request read
  path takes no lock. Registration fails on invalid identifiers, unknown
  roles, or a key already owned by another module; identical re-registration
  by the same owner is a no-op so tests can build the app repeatedly.
- Platform-wide entries stay central: `permission.denied`, `feature.disabled`,
  `rate_limit.exceeded`, `api.version_unsupported`, `system.*`, generic
  status titles, and validation messages; and the shared back-office
  permissions (`admin:users:*`, `admin:files:*`, `admin:stats:read`) that the
  admin module and the identity/files services both check.
- `internal/bootstrap` is the only package besides `cmd/*` that imports
  several modules. `test/arch` uses the same `bootstrap.Build`, so route
  authorization, audit coverage, group-middleware overlap, and SQL table
  ownership are checked for every registered module.
