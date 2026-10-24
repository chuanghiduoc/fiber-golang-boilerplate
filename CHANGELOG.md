# Changelog

All notable changes to this project will be documented in this file.

Format based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Changed
- Module self-registration for 100+ modules: `app.PermissionProvider`, `app.ErrorProvider`, `app.LocaleProvider`, and `app.FlagProvider` with `authz.Register`, `apperror.Register`, `features.Register`, and `i18n.New(locale, sources...)` (fail on duplicate/conflicting keys, invalid identifiers, unknown roles, and en/vi parity). Module permissions, error codes, flags, and strings moved to `internal/modules/<m>/catalog/` with unchanged string values; only cross-module/platform entries stay central. `internal/bootstrap` holds the single ordered module list built from `app.Infra` via each module's `Build`; `cmd/api` registers catalogs before validating `FEATURE_FLAGS` (now syntax-only in `config.Load`), building i18n, and serving. Webhook delivery/dispatch jobs register through `app.JobProvider`.
- Contracts split per provider: `internal/contracts/{identity,files,notifications,webhooks}`.
- `make migrate-create module=<owner> name=<purpose>` creates UTC-timestamp migrations; `test/arch/migrations_test.go` rejects duplicate versions, missing pairs, and unknown owners.
- `test/arch` builds every module from `internal/bootstrap` and checks audit coverage, `authz.Require` on every non-public `/api/v1` route, cross-module group-middleware overlap, and module SQL table ownership.
- `scripts/new-module.sh` generates catalog/providers/`Build`/tests/queries/sqlc block and leaves one manual step (bootstrap list).
- API consistency: every authenticated module route uses `app.Deps.Authenticate()` (API key or JWT; `authz.Require` gates both), so webhooks, notifications, and feature-flag admin now accept scoped API keys; `POST /auth/logout-all` stays JWT-only and now requires `sessions:revoke`. `POST /notifications/send` returns `201`. Webhook endpoint/source create/update/delete/rotate-secret and delivery replay, plus `POST /notifications/send`, are guarded by a required `audit.Guard` (503 when the audit store is unavailable); webhook domain events link to the intent row. Webhook `rotate-secret` no longer uses the idempotency cache, so a plaintext secret is never cached; a retried rotation rotates again. API-key rotation now rejects a caller lacking any of the key's scopes. Cursor building is shared in `pagination.NextCursor`.

### Added
- `jobs.WithKinds` / `jobs.WithExcludeKinds` claim filters for dedicated slow-kind workers, and the `jobs_queue_lag_seconds{kind}` histogram measured by the claim query.
- Transactional, fail-closed audit: `audit_events` gains parent/actor/source/outcome/status/error/idempotency/completion columns, tenant/request/parent and started-row indexes, plus a completion-only trigger that keeps terminal rows immutable. `audit.TxRecorder` writes Tier 1 domain events inside the mutation transaction (identity registration/profile/password/delete, admin role/ban/unban, admin file delete/restore); an audit failure rolls back and returns `503 system.audit_unavailable`. Tier 2 mutating routes use `audit.Guard` after idempotency to write a pre-write `started` intent row and complete it as succeeded/failed/denied (panic-safe), returning the same 503 when a required intent cannot be stored. Adds request-context helpers, metadata sanitization (secrets/PII redaction and size/depth caps), `audit_intent_failures_total`, `audit_completion_failures_total`, `audit_tx_missing_total`, and `audit_domain_missing_total`, and a route-coverage test proving every mutation is Tier 1 or guarded.
- Internationalization (`internal/platform/i18n`): embedded English and Vietnamese JSON catalogs, Accept-Language negotiation with q-values, case-insensitive matching, region truncation, wildcards, and `I18N_DEFAULT_LOCALE` fallback; global middleware sets `Content-Language` and `Vary: Accept-Language`; `{name}` interpolation; startup key/placeholder parity validation. Error responses localize catalog titles and validation messages through an optional `apperror.Localizer`; `AppError.DetailKey` opts details into translation while `code`/`type`/`status`/`instance` stay unchanged, and a nil localizer preserves existing behavior. Validation field errors now carry hidden min/max params, and default CORS exposes `Content-Language`.
- API versioning (`internal/platform/versioning`): explicit `V1`/`V2` versions, lifecycle `Policy`/`Config` (`DeprecatedAt`, `SunsetAt`, `Successor`, `DocsURL`), `Accept-Version` parsing (`1|v1|2|v2`) with mismatch returning `406 api.version_unsupported`, version middleware emitting `API-Version`, `Vary`, `Deprecation`, `Sunset`, and `Link`, and a guard returning 406 for unknown `/api/vN`. Modules can implement `app.V2Module` to opt into `/api/v2`; v1 remains always mounted, v2 is absent unless a module opts in, nil versioning config means v1 with no deprecation, and Swagger remains on v1.
- Compile-time feature flags (`internal/platform/features`) with a code-owned key catalog, `FEATURE_FLAGS`/`FEATURE_FLAGS_FORCE_OFF` config, optional PostgreSQL store, in-process snapshot cache with TTL and max-staleness fallback, precedence `force-off > kill switch > user target > tenant target > database default > environment > compiled default`, an `Evaluator` interface (`Enabled`/`Evaluate`/`Invalidate`), and Prometheus metrics. `app.Deps.Flags` is nil-safe (app installs an env/compiled-default evaluator) and is available to every module.
- Feature-flag admin module (`internal/modules/features`): `GET/PUT/DELETE /api/v1/admin/features/flags[/{key}]` and `GET/PUT/DELETE .../targets` with tenant-scoped targets, `admin:features:read`/`admin:features:write` permissions, audit metadata, idempotency, DTOs, repository/queries/sqlc, and unit/integration tests. `feature_flags` holds process-wide defaults and kill switches; `feature_flag_targets` carries `tenant_id` + nullable `user_id` with a composite FK to `users(tenant_id, id)`. Baseline migration, down migration, and `cmd/migrate` required tables/columns/indexes updated, plus config validation, docs, and operations guidance.
- Optional durable scheduler (`internal/platform/scheduler`): interval and 5-field UTC cron schedules (lists/ranges/steps and `@hourly`/`@daily`/`@weekly`/`@monthly`/`@yearly`), one PostgreSQL leader lease (`scheduler_leases`), a unique `scheduler_runs` run ledger, transactional enqueue, `none`/`latest`/`all` catch-up with a per-pass limit, run-ledger retention, graceful step-down/shutdown, and Prometheus metrics. Configure with `SCHEDULER_ENABLED` plus lease TTL/heartbeat/poll/catch-up/retention settings; `cmd/api` starts it only when enabled and stops it before the worker.
- Typed in-process event bus (`internal/platform/events`) with `Publish`, `PublishLocal`, and transaction-aware `PublishTx`: metadata carries event id/name/tenant/request/actor, local handlers run synchronously with panic isolation and `errors.Join`, and durable subscriptions enqueue one idempotent job per event and handler (`event:<id>:<handler>:<tenant>`). Adds `requestctx` request/actor propagation, `jobs.EnqueueOnce`/`EnqueueOnceTx`, optional `app.EventProvider`/`JobProvider`/`ScheduleProvider` hooks, and event/scheduler metrics.
- Tenant-scoped API keys (`internal/platform/apikey` + `internal/modules/apikeys`): `Authorization: ApiKey prefix_<32-byte base64url>` credentials store only a SHA-256 hash, bind to a tenant/user and an explicit `authz.Permission` scope set, support expiry/revocation, 24-hour rotation grace, throttled `last_used_at` tracking, and immediate owner revalidation through the identity `contracts.PrincipalReader` adapter. Admin lifecycle routes (`/admin/api-keys` list/create/get/update/delete/revoke/rotate) are guarded by `admin:api_keys:*`, tenant-scoped, and idempotent except secret-returning create/rotate (never cached), and attach non-secret audit metadata. Successful authentication sets `user_id`, `tenant_id`, `api_key_id`, and `auth_method`; rate-limit buckets are keyed by API-key id; an explicit conflicting `X-Tenant-Slug` returns 403. `api_keys` is part of the baseline schema and `cmd/migrate` verifies it.
- Attribute-based authorization (`internal/platform/authz`): `Subject` carries explicit permissions, cross-tenant capability, and attributes; `Policy`/`Decision`/`PolicySet` provide deny-overrides evaluation; `TenantPolicy`/`OwnerPolicy` conceal cross-tenant/foreign-owner resources as 404; `Authorize` performs an RBAC pre-check and maps deny to 403. Files and admin-file services enforce tenant/owner policies, routes now require `files:read`/`files:write`/`files:delete`/`files:restore`, `profile:read`/`profile:write`, and admin permissions, and the new `files:restore` permission is granted to users.
- Notification center: `internal/modules/notifications` owns a tenant/user-scoped in-app feed (`GET /notifications`, `GET /notifications/unread-count`, `GET /notifications/:id`, `POST /notifications/:id/read`, `POST /notifications/read-all`) and per-type `in_app`/`email` preferences (`GET`/`PUT /notification-preferences`). Missing preferences mean both channels are enabled; writes use `Idempotency-Key` and the generic audit middleware. The admin-only `POST /notifications/send` requires `notifications:send`; user routes use the new `notifications:read`/`notifications:write` permissions. Other modules create notifications through the stdlib-only `internal/contracts` `Notifier`, and identity supplies recipient email through `NotificationRecipientLookup`, so no module imports another module. The email channel enqueues the existing durable `email.send`/`email.send.encrypted` jobs with simple text/HTML; the in-app row and job commit atomically (an enqueue failure rolls the row back), inherit `jobs.tenant_id`, and are delivered at-least-once. Adds `notifications` and `notification_preferences` to the baseline schema with tenant-leading feed/unread indexes and a unique preference index; `cmd/migrate` verifies the new tables and index.
- Multi-tenant foundation: `tenants` table (seeded `default` tenant id 1), `tenant_id` columns and tenant-leading indexes on users, files, refresh/reset/verification tokens, jobs, and audit events; per-tenant email/Google uniqueness and last-admin trigger. Requests resolve `X-Tenant-Slug` (empty -> `TENANT_DEFAULT_SLUG`, invalid/unknown -> 404) through `internal/platform/tenant`; JWTs carry `tenant_id`, authenticated requests use the claim, and a conflicting explicit header returns 403 `permission.denied`. Adds `TENANT_DEFAULT_SLUG`/`TENANT_REQUIRE_HEADER` config and the identity tenant repository/resolver.
- Tenant-isolated identity: all identity SQL is scoped by `tenant_id`; registration/login/refresh/reset/verification/OAuth use the request tenant; per-tenant emails/Google IDs; tokens carry tenant; suspended/legacy tenantless tokens are rejected; admin user operations/stats/last-admin are tenant-scoped. Adds `tenantId` to `UserResponse`.
- Files tenant isolation: every file metadata query and admin list/count/stats query filters `tenant_id`; cross-tenant file ids return 404; delete/restore never cross tenants; new blobs use `<tenantID>/<userID>/<uuid>.<ext>` (old keys remain readable); `FileResponse`/`contracts.File` carry `tenantId`.
- Tenant propagation for durable jobs and audit: enqueue writes `jobs.tenant_id` from context, claim returns it, the worker restores tenant context for handlers; `audit.Event`/`PostgresRecorder` persist the request tenant. Job encryption/retention and metric labels are unchanged.
- Tenant isolation hardening: browser OAuth start accepts a validated `?tenant=` and binds it into signed `state`; the callback ignores the header, is exempt from tenant middleware, and rejects unknown/suspended tenants. Reset/verification links carry `?tenant=<slug>` with a validated query fallback on those auth endpoints. Startup/seed check tenant existence separately from active resolution, so a suspended default tenant no longer blocks boot. Idempotency keys are tenant-scoped; logout revokes refresh tokens by possession; production requires `TENANT_REQUIRE_HEADER=true`; non-positive JWT tenant claims are rejected; `cmd/migrate` verifies tenant columns, slug uniqueness, and tenant-leading indexes.
- Outbound and inbound webhooks: stdlib-only `internal/contracts/webhooks` event/dispatcher types; `pkg/webhook` HMAC-SHA256 signing/verification with timestamp tolerance and an SSRF-safe HTTP client; tenant-scoped endpoint CRUD with versioned AES-256-GCM secrets and rotation; delivery history, attempts, and replay; durable `webhook.delivery` jobs with payload/URL/secret snapshots, exponential backoff, `Retry-After`, permanent 3xx/4xx handling (except 408/425/429), and dead-lettering; inbound source CRUD and a public HMAC receiver deduplicated per `(source, X-Webhook-Id)`. Identity registration and file upload/delete emit `user.registered`, `file.uploaded`, and `file.deleted` through the contracts dispatcher (dispatch failures never fail the committed mutation). Adds `webhook_endpoints`, `webhook_endpoint_secrets`, `webhook_deliveries`, `webhook_delivery_attempts`, `webhook_inbound_sources`, and `webhook_inbound_events` (all `tenant_id`-scoped), `webhooks:read`/`webhooks:write` permissions, audit metadata, retention, and `WEBHOOK_*` configuration. See `docs/webhooks.md`.
- Webhook configuration validation: dedicated `WEBHOOK_SECRET_ENCRYPTION_KEY` (JWT fallback outside production), connect/request timeouts, retry base/max, attempt cap, retention window, inbound timestamp tolerance, and SSRF allowlist parsing.
- Webhook tests: HMAC signing/verification, SSRF classification, client redirect/timeout classification, retry/backoff and `Retry-After`, dispatcher fanout/dedupe, worker success/permanent/retry outcomes, secret codec round-trip, and a PostgreSQL-backed end-to-end suite covering tenant isolation, rotation/idempotency, dispatch, delivery outcomes, and inbound signatures.
- Canonical module template (`docs/module-template.md`) and Bash scaffold (`scripts/new-module.sh`): compile-safe `module.go`, handler/service/repository/DTO layout, route/permission/audit/idempotency conventions, table ownership and migration rules, cross-module contracts, tests, and an add-a-module checklist; the script validates lower_snake_case, rejects Go keywords/reserved directories, supports `--dry-run`, refuses overwrites, runs `gofmt` when available, and verifies the generated package with `go build`
- Operations runbook (`docs/operations.md`): health/readiness, Prometheus metrics, structured logs and request IDs, job retry/dead-letter/retention/heartbeat/drain behavior with the JWT rotation caveat, idempotency/Redis TTLs and fail-open behavior, migration and legacy-upgrade guidance with backup/restore notes, Kubernetes ordering/HPA/NetworkPolicy/SMTP/S3/Redis caveats, and incident checklists for DB/Redis/SMTP/job backlog
- Kubernetes production foundation under `deploy/k8s` (namespace, ConfigMap, Secret template, one-shot migration Job, Deployment, Service, HPA, PDB, NetworkPolicy, Ingress) and a GHCR multi-arch release workflow with semver/SHA tags, provenance/SBOM, and CI manifest validation
- Modular-monolith composition root (`internal/app`) with module-owned route tables (`internal/modules/identity`, `files`, `admin`); platform packages for cross-cutting mechanics
- Durable PostgreSQL job queue (`internal/platform/jobs`): transactional enqueue, `FOR UPDATE SKIP LOCKED` claiming, lease heartbeats with stale-lock reclamation, exponential backoff, dead-lettering, graceful drain; email delivery now runs through queued jobs. Password-reset and verification tokens are committed together with their `email.send` job via `QueuedSender.SendTx`, so an enqueue failure rolls the token back. Registration is atomic too: the user row, verification token, and `email.send` job commit in one transaction, and registration fails closed if the outbox write fails (no user is created). Resend-verification and email-change flows still provision their token and email separately after the account exists. Delivery is at-least-once, so handlers must be idempotent.
- Prometheus job and audit metrics: `jobs_claimed_total{kind}`, `jobs_succeeded_total{kind}`, `jobs_retried_total{kind}`, `jobs_dead_total{kind}` (handler dead-letters only; lease-expiry dead letters are table-visible but not counted), `jobs_claim_errors_total`, `jobs_purged_total`, and `audit_write_failures_total`
- Automatic terminal-job retention: workers purge `succeeded` and `dead` rows older than 30 days (hourly by default, first pass on startup, 1,000-row batches under a 5s bound) via `WithRetention`/`WithPurgeInterval`; queued and running rows are never deleted
- Encrypted email job payloads at rest: `jobs.PayloadCodec` derives an AES-256-GCM key from `JWT_SECRET` with HKDF-SHA256 and seals `jobs.payload`, so reset/verification links are not readable in a database dump, backup, or replication stream while a job is queued. `EmailHandler` still accepts legacy plaintext JSON from jobs queued before deployment; those rows remain readable at rest until delivered or cleared, and external tools see the envelope in `jobs.payload`. Encrypted jobs use the distinct kind `email.send.encrypted`; new workers register both kinds, so during a rolling deploy old workers leave encrypted jobs queued as unknown-kind retries instead of misreading them, and the new workers drain them. Rotating `JWT_SECRET` makes already-queued encrypted jobs undecryptable; drain the queue before rotating it.
- Opt-in `Idempotency-Key` support on mutating endpoints: 2xx responses replay for 24 hours, concurrent duplicates get 409, cache failures fail open, and token/credential endpoints plus GET/HEAD/OPTIONS are excluded (Redis is recommended for multi-replica deployments). Keys are body-bound for non-multipart payloads up to 1 MiB (different body -> 409), an owner-token lock is held for 2 minutes, replays expose `Idempotent-Replayed: true`, audit skips replayed mutations, and the default CORS allow list includes `Idempotency-Key`
- Permission-based authorization (`internal/platform/authz`) with a role→permission matrix and `Require`/`RequireAny` middleware
- Generic audit trail (`internal/platform/audit`) for successful mutating requests plus explicit domain events
- Typed error catalog (`pkg/apperror/catalog.go`) mapping stable codes to HTTP status/title
- One-shot database entrypoint `cmd/migrate`; Docker Compose runs it before the API
- Architecture test (`test/arch`) that rejects cross-module and platform→module imports
- `make check` and `make migrate-run`

### Changed (architecture)
- API and seed no longer auto-run migrations; only `cmd/migrate` applies DDL, preventing replica migration races
- Consolidated all schema into a single `migrations/000001_init_schema.up.sql` / `.down.sql` (replaces the old shipped migrations `000001`–`000003`); `jobs`, `audit_events`, `scheduler_leases`, and `scheduler_runs` live in that file, and `jobs` carries a nullable `idempotency_key` with a partial unique `(tenant_id, idempotency_key)` index
- Dockerfile builds both `server` and `migrate`; Compose adds a one-shot `migrate` service and app dependency
- `make run` migrates before starting; `make seed` depends on migration
- CI builds both binaries, runs `go vet`/`gofmt`, and includes a fresh-PostgreSQL migration job (baseline up/down + `cmd/migrate` twice)
- README/CLAUDE/CONTRIBUTING document the module layout, dependency rules, schema ownership, and upgrade caveats
- Moved the files upload service, HTTP handler, repository, and DTO into `internal/modules/files/{service,handler,repository,dto}`; no compatibility aliases remain
- Moved the identity bounded context into `internal/modules/identity/{dto,repository,service,handler}`. Identity handlers use `internal/platform/httpx` helpers, shared cursor pagination lives in `pkg/pagination.DBCursor`, unique-violation detection lives in `pkg/database`, and the legacy global identity files were deleted.
- Moved the admin bounded context into `internal/modules/admin/{dto,service,handler}`. Admin now depends only on the stdlib-only `internal/contracts.IdentityAdmin`/`FilesAdmin` interfaces implemented by identity/files adapters, composes dashboard stats from owner-scoped `AdminUserStats`/`AdminFileStats` queries, and no longer imports module repositories, DTOs, sqlc, storage, database, or sessions. The legacy admin handler/service, admin DTO, and cross-module `GetSystemStats` query were deleted.
- Removed the final legacy packages (`internal/handler`, `internal/service`, `internal/dto`) and their compatibility aliases. All bounded-context DTOs, repositories, services, and handlers are module-owned; upload tests and helpers now live beside the files module code. `admin` composes through `internal/contracts`; generated sqlc code was still shared at that point.
- Moved generated sqlc code into module-scoped packages: `internal/modules/identity/{queries,sqlc}` and `internal/modules/files/{queries,sqlc}`, configured by one `sqlc.yaml` block per module. Deleted the shared `internal/sqlc` package and root `queries/` directory. Repositories, services, seeds, and tests now import `identitysqlc`/`filesqlc`, the session validator reads role/token version through a narrow repository method instead of a generated type, and the architecture test rejects cross-module sqlc imports.

### Fixed
- Kubernetes ConfigMap CORS now allows the `Idempotency-Key` request header and exposes `Idempotent-Replayed`, so browser idempotency works behind the template policy
- `MemoryCache.Increment` now rejects non-integer values like Redis `INCR`, so an idempotency owner token is never mistaken for a free claim counter
- Worker claim errors are retried with exponential backoff capped at 30s instead of terminating the worker (and the API process that hosts it)
- Job heartbeat, success, and failure writes are fenced by `attempts`, so a worker whose lease was reclaimed cannot finish the job under its new owner
- Login, register, and refresh audit events now carry the authenticated user ID instead of an anonymous actor
- Handlers get a 10m default timeout (`WithJobTimeout`, non-positive disables it), and unknown job kinds now retry with normal backoff until `max_attempts` before dead-lettering
- Job bookkeeping now reports lost leases (`RowsAffected` check) and logs outcome-write failures instead of silently leaving a job `running`
- Job outcome metrics advance only after the outcome write succeeds, so a bookkeeping failure is not reported as a successful retry, dead-letter, or success; audit write failures are counted and logged
- Heartbeats are clamped to at least four renewals per lease, and terminal jobs clear their payload so queued email secrets are not retained
- `cmd/migrate` verifies required tables after migrating and fails with an actionable message for legacy version-1 schemas
- Migration jobs enforce the same production TLS rule as the API (`DB_SSLMODE=disable` is rejected)
- Compose forwards SMTP/email-from settings so a production SMTP deployment can start without editing the file
- The baseline schema drops/recreates triggers idempotently; the down migration guards against a missing `users` table

### Upgrade notes

- **Upgrading an existing database:** the consolidated `000001_init_schema` is
  a fresh-install baseline, not a drop-in replacement for a database that
  applied the old migrations. Keep its `schema_migrations` history, restore the
  old `000002`/`000003` source files from git history (golang-migrate requires
  the recorded version to exist), and add a new numbered migration (a fully
  migrated old deployment is at version 3, so `000004` or later) with any
  missing tables/indexes, such as `jobs` and `audit_events`. Do not point an
  existing database at the new `000001` or reset `schema_migrations`, and never
  edit a shipped migration after deployment. `cmd/migrate` now prints this
  guidance for legacy history and verifies required tables, columns, and unique
  indexes after migrating. See
  "Upgrading an existing database" in the README.
- **Feature flags on an existing database:** add `feature_flags`,
  `feature_flag_targets`, their partial unique indexes, and a unique index on
  `users(tenant_id, id)` (the parent key for tenant-safe user targets) in the
  next numbered migration. Without the optional database store the evaluator
  still runs on `FEATURE_FLAGS`/compiled defaults, but the admin API fails until
  the tables exist; `cmd/migrate` reports the missing objects.
- **Webhook deployment:** set `WEBHOOK_SECRET_ENCRYPTION_KEY` (32+ random
  characters) in production before starting the API; local/test fall back to
  `JWT_SECRET`. Existing databases need the six `webhook_*` tables added by a
  numbered migration (or are fresh installs of the updated baseline). Rotating
  the encryption key later makes stored webhook secrets unreadable, so rotate
  every outbound/inbound secret after the key change. Empty
  `WEBHOOK_SSRF_ALLOWLIST` keeps private/loopback targets blocked.
- **Notifications on an existing database:** add `notifications` and
  `notification_preferences` (with their tenant-leading and unique indexes) to
  the same upgrade migration. See "Upgrading an existing database" in the
  README.

### Changed (API contract — breaking)
- Errors now follow RFC 9457 Problem Details (`application/problem+json`): `type`, `title`, `status`, `code` (stable dotted domain code), `detail`, `instance`, `requestId`, `timestamp`, and a flat `errors[]` for validation. Configurable docs base via `ERROR_DOCS_BASE_URL`; typed codes use their catalog `DocPath` for `type`.
- Successful responses return the resource directly (no `{data,meta}` envelope). List endpoints use the Stripe-style `{object:"list", url, data, hasMore, nextCursor}` envelope.
- List endpoints switched to forward-only cursor pagination (`limit` + `startingAfter`), backed by `(created_at, id)` indexes; the opaque cursor is `base64url(created_at:id)`. Offset helpers remain for small tables.
- JSON keys are camelCase; correlation header standardised to `X-Request-Id`.

### Security
- Consume refresh tokens atomically (`DELETE … RETURNING`) so a replayed/concurrent refresh cannot mint two sessions
- Bind refresh tokens to the authenticated token version and require a matching version at creation/consumption, closing the bcrypt/rotation race with password changes
- Add a `token_version` JWT claim plus a generation-keyed session cache: password changes/resets, bans, unbans, and role changes invalidate existing access tokens immediately without stale write-after-invalidate snapshots
- Track `password_changed_at` and reject reset links issued before the last password or email change
- Bind email-verification tokens to the address they were sent to and require the address to still match at verification time
- Add `POST /auth/logout-all` to revoke every refresh token and bump the token version, invalidating access tokens immediately
- Add admin file takedown (`DELETE /admin/files/:id`) and owner `includeDeleted=true` listing for trash/restore
- Distinguish admin bans from self-deletion (`banned_at`): banned emails cannot be reused through registration or Google login
- Share rate-limit counters through the application cache (Redis in production); log/metric error responses with their real status instead of 200
- Mark the seeded admin email verified so `REQUIRE_EMAIL_VERIFICATION=true` does not lock out a fresh deployment; disable Swagger UI in production and give docs a compatible CSP
- Normalize emails (trim + lowercase), enforce case-insensitive uniqueness among active users, and allow email reuse after soft delete
- Reset `email_verified_at` on email change, require the current password for self-service email changes, invalidate old verification tokens, and send a fresh link
- Revoke every refresh token and bump the token version on password change
- Remove the public `/uploads*` static route; files are only served through authenticated download endpoints, with a MIME-derived storage extension
- Require `verified_email=true` from Google and refuse to auto-link Google to an unverified local account
- Redact OAuth/token query parameters from request logs; validate client-supplied `X-Request-Id`
- Pin JWT parsing to HS256, require `exp`/`iat`, and add clock-skew leeway
- Reject self-demotion/self-ban and last-admin removal; bump token versions on role changes
- Protect the final admin and repair partially-seeded admins on startup
- Hash password-reset and email-verification tokens (SHA-256) before storing, matching refresh tokens — a DB leak can no longer be replayed
- Upgrade SMTP connections via STARTTLS when advertised; add `SMTP_REQUIRE_TLS` to fail-close when the server offers no TLS
- Protect `/metrics` behind an optional `METRICS_AUTH_TOKEN` bearer (constant-time compare); drop the `Server` response header
- Add `APP_TRUST_PROXY`/`APP_TRUSTED_PROXIES` so `c.IP()` (rate-limit key) cannot be spoofed via `X-Forwarded-For`
- Make the login-attempt counter atomic (`Cache.Increment`) so parallel requests cannot bypass brute-force lockout
- Harden `Content-Disposition` to RFC 6266/5987 (strip control/path chars, encode Unicode via `filename*`)

### Fixed
- `TxManager.WithTx` now always rolls back (including on panic), preventing pooled-connection leaks
- Password-reset requests are rate-limited before lookup and always return the same response, removing the account-enumeration oracle
- Email-verification tokens are single-use and email-bound; sibling password-reset tokens are invalidated after a successful reset
- Local uploads are written to a temporary file, size-checked, and atomically renamed; DB-failure cleanup uses a non-canceled context
- `APP_BODY_LIMIT` default now exceeds `STORAGE_MAX_FILE_SIZE`, so advertised 10 MB uploads are reachable
- DSN construction escapes credentials/database names; migrations resolve paths and schemes robustly
- Server exits non-zero when `Listen` fails; startup is bounded by a timeout; HTTP read/write/idle timeouts are configured
- The consolidated schema preserves email reuse after soft delete, creates real unique token-per-user indexes, and adds a DB trigger that makes last-admin removal safe under concurrency
- The consolidated schema includes `banned_at`; migrations are no longer edited after shipping
- Rate-limit tiers use separate keyspaces, and cache-backed limiter reads/writes fail open instead of turning a Redis blip into API-wide 500s
- Console email logs HTML-only messages (reset/verification links), and seeded admins are marked verified
- S3 downloads stat the object before responding and bound/cancel the stream; missing objects no longer surface after a 200
- Integration tests can run without Docker via `TEST_DATABASE_URL` using an isolated schema
- JSON bodies reject unknown fields and trailing values; duplicate emails return 409, oversized uploads 413, and unsupported media 415
- Non-owned user/file IDs return 404 instead of 403 so sequential IDs cannot be used to confirm another user's resources exist
- `TxManager.WithTx` now uses `errors.Join` on rollback failure so `errors.Is/As` still see the original business error
- Fix cross-platform path-traversal false positive: canonicalize the local storage base path (symlink/8.3 resolution)
- Readiness returns HTTP 503 when degraded; readiness checks now run truly in parallel
- `userService.Delete` and OAuth flows no longer swallow refresh-token revocation errors
- `token.Parse` returns an explicit error when the token is invalid; `email.NewSender` rejects unknown drivers
- `MemoryCache.Close` is idempotent (`sync.Once`); Redis `Increment` sets TTL atomically via a Lua script

### Improved
- Consolidate the schema into a single `000001_init_schema` migration with `CHECK` constraints on `role`/`auth_provider`
- Remove sqlc types from service APIs (`Authenticate`/`FindOrCreateByGoogle` return DTOs, `Download` returns a DTO, `Verify` returns the user ID) — cleaner layer boundary
- Validate DB pool sizing, cache/email drivers, and trusted-proxy configuration at startup

## [1.0.1] - 2026-02-28

### Fixed
- Log storage cleanup failure instead of silently ignoring in upload service
- Log cache errors in login lockout instead of silently ignoring
- Return email send error from SendVerification so callers can handle it
- Log refresh token revocation failure when banning users
- Fix down migration drop order (function before table)
- Fix password validator to use rune count for min length (Unicode support)

### Security
- Add Content-Security-Policy header to security middleware
- Add context-aware timeout (30s) to SMTP sender via net.DialContext
- Add production warnings in .env.example for CORS, DB_SSLMODE, JWT_SECRET, admin credentials

### Improved
- Mock repos now enforce email uniqueness, track email content, TTL, and deleted paths
- Add resource limits (memory/CPU) to all Docker Compose services
- Add Dependabot for Go modules, GitHub Actions, and Docker base images

## [1.0.0] - 2026-02-23

### Added
- Auth: register, login, refresh, logout, forgot/reset password, email verification, Google OAuth
- Users: CRUD, profile, change password, admin list
- Files: upload, download, list, soft delete (local/S3/MinIO storage)
- Admin: stats, user management (ban/unban/role), file listing
- JWT with iss/aud claims, refresh token rotation
- Pluggable drivers: storage (local/s3/minio), cache (memory/redis), email (console/smtp)
- Initial schema setup and admin seeding at startup (superseded by the one-shot `cmd/migrate` and `cmd/seed` commands)
- Swagger/OpenAPI docs
- Prometheus metrics, structured slog logging, health checks
- Tiered rate limiting (strict/normal/relaxed)
- CI pipeline (lint + test), Dockerfile multi-stage build
- GitHub issue templates, module rename script
- Unit tests for service layer, token, validator
