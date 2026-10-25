# Changelog

All notable changes to this project will be documented in this file.

Format based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Added (P3b API compatibility and data retention)
- CI `API compatibility` job (ADR 0006, pull requests only): `cmd/openapi3` (kin-openapi v0.149.0 `openapi2conv`) converts the base branch's and the PR's `docs/swagger.json` to OpenAPI 3, and `oasdiff breaking --fail-on ERR` (oasdiff v1.33.0) fails the PR on breaking changes. Editing `docs/api-breaking-changes.md` in the same PR acknowledges an intentional break (the report is still printed).
- `files.trash.purge` (hourly, `files` `ScheduleProvider`/`JobProvider`): files soft-deleted more than `FILES_TRASH_RETENTION_DAYS` (default 30, 1..3650) ago are permanently removed across tenants, blob first and then the row, in 100-row `FOR UPDATE SKIP LOCKED` batches (a concurrent restore waits, then gets 404). A missing blob counts as deleted; a failed blob delete keeps its row and fails the job after the rest commits. No domain event or webhook is emitted.
- `notifications.retention.purge` (hourly): notifications **read** more than `NOTIFICATIONS_RETENTION_DAYS` (default 180, 1..3650) ago are deleted across tenants in 1,000-row batches; unread notifications are never purged.
- Both purges require `SCHEDULER_ENABLED=true` (like `identity.tokens.purge_expired`); without it nothing is purged.
- Migrations `20261025112300_files_trash_purge_index` (partial `idx_files_deleted_at`) and `20261025112400_notifications_retention_index` (partial `idx_notifications_read_at`), both `CREATE INDEX CONCURRENTLY`.
- `notifications.Build` takes `notifications.Settings`; `files.Settings` gains `TrashRetentionDays`; `config.RetentionConfig` (`cfg.Retention`).

### Breaking (P2c event envelope, Standard Webhooks, idempotency 422)
- **Webhook wire format is now [Standard Webhooks](https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md)** (ADR 0009). Headers `webhook-id` (event id, stable across retries), `webhook-timestamp`, `webhook-signature: v1,<base64 HMAC-SHA256>` over `<webhook-id>.<webhook-timestamp>.<raw body>`, keyed with the base64-decoded secret after `whsec_`; during the 24h rotation grace two signatures are **space**-separated. `X-Webhook-Id`, `X-Webhook-Delivery`, `X-Webhook-Event`, `X-Webhook-Timestamp`, `X-Webhook-Signature` (`v1=<hex>`), and `X-Webhook-Attempt` are no longer sent; `User-Agent` is `fiber-golang-boilerplate-webhooks/2.0`.
- **Webhook bodies are the versioned event envelope** (ADR 0011): `{id, type, dataVersion, occurredAt, tenantId, actor: {type, id}, data}` replaces `{id, type, createdAt, data}`. Event ids are UUIDv7 (previously `evt_<hex>`; replays were `replay.<id>.<hex>`, now a fresh UUIDv7). Payloads are new contract structs, not REST DTOs: `user.registered` → `{userId, email, name, emailVerified, createdAt}` (no `role`, `hasPassword`, `tenantId`, `updatedAt`); `file.uploaded` → `{fileId, ownerId, originalName, mimeType, size, createdAt}` (no `url`, `tenantId`); `file.deleted` → `{fileId, ownerId, originalName, deletedAt}`; `file.restored` → `{fileId, ownerId, originalName}`. All are `dataVersion` 1.
- **Receiver migration:** (1) verify with an official Standard Webhooks library or `webhook.DecodeSecret` + `webhook.VerifyRequest` (examples in `docs/webhooks.md`); (2) deduplicate on `webhook-id` instead of `X-Webhook-Delivery`; (3) read the event type from the body's `type` and branch on `dataVersion`; (4) map the renamed data fields above; (5) **rotate every endpoint secret** (`POST /webhooks/endpoints/:id/rotate-secret`) and update the receiver: secrets are now `whsec_<standard base64 of 32 bytes>`, and deliveries whose snapshot secret predates the format dead-letter with `endpoint secret predates Standard Webhooks; rotate it` (replay them after rotating). There is no dual-format period.
- **Inbound receiver** (`POST /webhooks/inbound/:id`) verifies the same scheme: senders must send `webhook-id`/`webhook-timestamp`/`webhook-signature` signed with the source secret, and put the event type in the JSON body's top-level `type` (stored as `inbound` when absent; a non-concrete type is `400`). `X-Webhook-Event` is ignored. New inbound secrets use `whsec_` (was `whin_`); rotate existing sources, whose old secrets now fail with `401` (logged).
- **Idempotency** (ADR 0008): reusing an `Idempotency-Key` with a different body returns `422` with code `idempotency.key_mismatch` (was `409`); a duplicate still in progress stays `409`. The new code is in the platform catalog with `en`/`vi` title and detail.
- **Producers publish domain events instead of calling webhooks** (ADR 0011): identity registration and file upload/delete/restore (user and admin) publish with `events.Bus.PublishTx` in their transaction; webhooks subscribes durably (`events.<type>.webhooks` jobs, 20 attempts). Removed: `internal/contracts/webhooks` (`Event`, `Dispatcher`, `Noop`; pattern helpers moved to `webhooks/service`), `app.Deps.Webhooks`, `bootstrap.Modules.Webhooks`, `webhooks.Provides`, `identity.Module.AttachWebhooks`, the `WebhookTxDispatcher` type assertions, `Dispatcher.DispatchTx`, and the `webhook.dispatch` job kind (no deployed queue exists; a queued `webhook.dispatch` job would retry as an unknown kind until dead). `files.Build`, `NewUploadService`, and `NewAdminFileService` no longer take a dispatcher; there is no post-commit fallback.
- `events.Event` is the envelope: `Name` → `Type`, `ActorID int64` → `Actor{Type, ID}`, new required `DataVersion`; `NewEvent(type, dataVersion, data)`; ids are UUIDv7; types must be lowercase `[a-z0-9._-]`; invalid durable payloads dead-letter (`jobs.Permanent`). `app.Infra.Events` carries the bus to module `Build`.
- `webhook_events` gains `data_version`, `actor_type`, `actor_id` (baseline `20260101000900_webhooks_init` edited in place: it has not shipped).

### Added (P2c)
- Event payload contracts `internal/contracts/identity/events.go` and `internal/contracts/files/events.go`; `webhooks/catalog.SubscribedEvents`.
- `pkg/webhook`: `NewSecret`, `DecodeSecret`, `SignatureHeader(sigs...)`, `SecretPrefix`, `ErrInvalidSecret`; `Sign`/`Verify`/`VerifyRequest` take the decoded key and drop the event type. Pinned to the specification's published test vector.
- Tests: an independent Standard Webhooks verifier in the delivery worker unit test and the wire-format golden test, and an end-to-end integration test (file upload → `PublishTx` → durable subscription on a real jobs worker → delivery → captured request verified).

### Breaking (P2a per-owner baseline and normalization)
- ADR 0005: `migrations/000001_init_schema.*` is split into ten per-owner baseline files `20260101000000_platform_functions` … `20260101000900_webhooks_init` (shared functions, identity, platform jobs/scheduler/audit, then apikeys, files, features, notifications, webhooks). There are no sequential versions any more; `test/arch` rejects them and also rejects a migration that creates another owner's table. A database recorded at version 1 is a legacy history: `cmd/migrate` refuses it with the upgrade guidance (`force 20260101000900` after manual DDL). No deployed database exists, so recreate local databases.
- `api_keys.scopes JSONB` → `api_key_scopes(api_key_id, permission_key)` (CASCADE, format `CHECK`; the RBAC phase adds the `permissions` FK). The API still returns a sorted `scopes` array.
- `webhook_endpoints.event_types TEXT[]` → `webhook_endpoint_subscriptions(tenant_id, endpoint_id, pattern)`; matching is unchanged, and `eventTypes` is now returned sorted.
- Webhook payloads are stored once in `webhook_events(tenant_id, event_id, event_type, occurred_at, payload)`; `webhook_deliveries` drops `event_type`/`event_created_at`/`payload` and references the event (RESTRICT). Retention deletes unreferenced events after terminal deliveries. Signed requests are byte-identical (`TestWebhookDeliveryWireFormatIsStable`).
- `webhook_delivery_attempts` references `(tenant_id, delivery_id)`; `audit_events.ip` is `INET` (non-addresses stored as NULL), `actor_type` no longer allows `admin` (admins are `user` + `actor_role = 'admin'`; `audit.ActorTypeAdmin` is removed), and `parent_id` references `audit_events(id)`.
- `feature_flag_targets` uses one `UNIQUE NULLS NOT DISTINCT (tenant_id, flag_key, user_id)` index; new `CHECK`s: `jobs_running_has_owner`, `api_keys_rotation_grace_matches_rotated`, `files.size >= 0`.

### Added (P1a generator)
- `cmd/gen` (`make gen module=|resource=|command=|query=`, `dry_run=1`, `no_http=1`): Go stdlib generator for ADR 0002 code. `module` creates the skeleton (`Build(infra, Settings) (*Module, Provides, error)`, catalog, `service/ports.go`, repository/handler/dto, sqlc config, tests, `<ts>_<module>_init` migrations) and registers it once in `internal/bootstrap/modules.go`; `resource` adds a tenant-scoped CRUD slice (migration, cursor queries, repository row mapping, service over ports, guarded routes, permissions, error code, en/vi strings, fake-based unit tests, a PostgreSQL integration test); `command`/`query` add `service/{command,query}_<name>.go` with an HTTP stub. Each run verifies with sqlc, `go build ./...`, `go vet` (with and without `-tags=integration`), the module tests, and `test/arch`, and restores every written file on failure. `scripts/new-module.sh` is now a wrapper for `go run ./cmd/gen module`.

### Changed (P1a generator)
- sqlc configuration is per module: `internal/modules/<name>/sqlc.yaml` (paths relative to it, `//go:generate sqlc generate -f sqlc.yaml` in `generate.go`) replaces the root `sqlc.yaml`. `make sqlc-generate` and the CI drift check run every module config; CI also fails when a module has `queries/` but no `sqlc.yaml`. Generated code is unchanged.

### Breaking (P0 foundation)
- Production requires `DATA_ENCRYPTION_KEY` (32+ characters, different from `JWT_SECRET`) for job payload encryption. Upgrade: set the new key and put the current `JWT_SECRET` value in `DATA_ENCRYPTION_PREVIOUS_KEYS` (same HKDF derivation, so queued v1 envelopes still open) until those jobs drain; outside production an empty key still falls back to `JWT_SECRET`.
- Job payloads and webhook secrets are sealed in envelope v2 (with `kid`); workers from earlier releases cannot open v2 and retry until `max_attempts`. Roll out every worker (including a separate worker Deployment) before relying on newly sealed jobs or creating/rotating webhook secrets. v1 envelopes still decrypt.
- `authz.Authorize` fails closed: a service call with neither a subject nor `authz.SystemContext` is `403`. apikeys `ensureCallerCanGrant` now denies callers without a subject. Module job handlers and durable event handlers run under `SystemContext` automatically; other in-process callers must mark their context.
- Fiber adapters moved to `<pkg>http` subpackages: `apperror.FiberErrorHandler`/`Localizer` → `apperrorhttp`; `tenant.Middleware`/`ID`/`Slug` → `tenanthttp`; `authz.Require`/`RequireAny`/`SetSubject`/`SubjectFromFiber` → `authzhttp`; `audit.Guard`/`Middleware`/`Policy`/`Mode*`/`AddMetadata` → `audithttp`; `features.Middleware` → `featureshttp`; `apikey.Authenticate` → `apikeyhttp`. `pkg/storage` keeps the interface and local driver; S3/SeaweedFS moved to `pkg/storage/s3store` and the factory `storage.NewStorage` to `pkg/storage/drivers.New`.
- `app.Infra.Config` is replaced by `Infra.Platform` (App, JWT, Tenant); identity, files, and webhooks take a module `Settings` struct in `Build`, and `bootstrap.Build` takes `(cfg, infra)`.

### Added (P0 foundation)
- `APP_ROLE=api|worker|all` (default `all`): `worker` runs jobs, the scheduler, and background loops with only `/healthz`, `/readyz`, `/metrics`; `api` serves HTTP without claiming jobs. `JOBS_WORKER_KINDS` / `JOBS_WORKER_EXCLUDE_KINDS` configure bulkhead workers. Optional Kubernetes component `deploy/k8s/base/worker/` (enabled by no overlay).
- Key rotation without downtime: JWTs carry a `kid` derived from the secret and `JWT_PREVIOUS_SECRETS` verifies retired keys (tokens without `kid` verify against the active key); `DATA_ENCRYPTION_PREVIOUS_KEYS` and `WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS` decrypt with retired keys. Rotation is two-phase (add as previous everywhere, then promote).
- `<NAME>_FILE` for secret settings (`JWT_SECRET`, `JWT_PREVIOUS_SECRETS`, `DB_PASSWORD`, `DATA_ENCRYPTION_KEY`, `DATA_ENCRYPTION_PREVIOUS_KEYS`, `WEBHOOK_SECRET_ENCRYPTION_KEY`, `WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS`, `SMTP_PASSWORD`, `GOOGLE_CLIENT_SECRET`, `ADMIN_PASSWORD`, `METRICS_AUTH_TOKEN`, `STORAGE_S3_SECRET_KEY`, `REDIS_URL`); setting both forms fails startup.
- `jobs.Permanent(err)` dead-letters a job immediately (`last_error` prefix `permanent failure (not retried): `, counted in `jobs_dead_total`); used for undecodable/invalid email, password-reset, and webhook payloads. Decryption failures still retry.
- `DB_QUERY_EXEC_MODE` (`cache_statement` default, `cache_describe`, `describe_exec`; `exec`/`simple_protocol` rejected) and `DB_MIGRATE_HOST`/`DB_MIGRATE_PORT` so `cmd/migrate` can bypass a transaction-mode PgBouncer; PgBouncer `search_path` handling documented.
- Architecture tests: `test/arch/framework_free_test.go` (cores and module service/repository packages never import Fiber/fasthttp; `pkg/storage` and `files/service` never import the AWS SDK) and `test/arch/layering_test.go` with the shrink-only `layering_allowlist.txt` (ADR 0002).
- ADRs in `docs/adr/` (index `docs/adr/README.md`), `.github/CODEOWNERS`, cosign keyless signing in `release.yml` with `cosign verify` in `deploy.yml` (when the image preflight runs), and digest-pinned Dockerfile base images.

### Changed (P0 foundation)
- Job payloads (email and password-reset) are sealed with `DATA_ENCRYPTION_KEY` instead of a `JWT_SECRET`-derived key, so rotating `JWT_SECRET` no longer requires draining the queue. Google OAuth `state` is still derived from the active `JWT_SECRET`.
- Identity service tests use `bcrypt.MinCost` through `service/main_test.go`; production cost stays 12.

### Fixed
- Jobs: each row stores `lease_expires_at` (now() + the owning worker's lease, set on claim and every heartbeat) and is reclaimed only when it is past, so workers with different `WithLeaseDuration` no longer reclaim each other's healthy jobs; a `CHECK` requires a lease on `running` rows.
- Webhooks: `WEBHOOK_CONNECT_TIMEOUT_SECS` now bounds the TLS handshake as well as the TCP dial.
- Redis client enables `ContextTimeoutEnabled`, so the 250ms/500ms per-operation bounds in rate limiting and idempotency actually apply against a stalled server.
- Idempotency lock is an atomic owner-token claim (`SET NX PX` via the new `cache.Cache.SetNX`); a lost reply re-reads the key (own token ⇒ held, other ⇒ `409`, missing/read error ⇒ fail open), and the lock is still released by compare-and-delete after failing open.
- Audit: client-supplied text and metadata are sanitized before storage (invalid UTF-8 → U+FFFD in text columns and dropped in `jsonb`, NUL stripped), so a crafted header cannot fail the insert; `status`/`path` are reserved fallback metadata keys.
- `APP_REQUEST_TIMEOUT` is capped at 12s (default 12; `.env.example`, Compose, and the ConfigMap use 12) so a request plus its 3s post-handler work (`config.PostHandlerReserve` = audit 2s + idempotency 1s) fits the shutdown drain window.
- Storage: S3 `NoSuchBucket` is a configuration error (`500`), not a missing blob. AWS returns `403` for a missing key when credentials lack `s3:ListBucket`; grant it so missing blobs map to `404`.
- Identity: `(*AppError).Is(ErrNotFound)` matches 404 `AppError`s; refresh with an inactive owner returns `401`; API-key rotate returns `409` when the owner is inactive or the key predates `credentials_revoked_at`; key creation locks the owner row via `PrincipalLocker.LockPrincipalTx`; ban stamps `credentials_revoked_at` with `clock_timestamp()`.
- Google sign-in (`SignInWithGoogle` with a `Complete` hook) runs tenant/email checks and session creation in the sign-in transaction before the `auth.google_login` audit row, so refused logins are not audited as success.
- The password-reset token insert re-checks the current email address; an email change in between skips the send.
- `cmd/migrate` lists missing schema objects in sorted order.
- PostgreSQL 18 reports a delete blocked by `ON DELETE RESTRICT` as `23001 restrict_violation` (not `23503`); `database.ForeignKeyViolation` accepts both, so deleting a feature flag that still has targets returns `409` instead of `500`.
- Cursor pagination no longer degrades on deep pages: every cursor query uses a plain `(created_at, id) < (...)` row comparison (first page binds `('infinity', MaxInt64)` via `DBCursor.CreatedAtParam`/`IDParam`) instead of `NOT has_cursor OR ...`, so the bound is an index condition under generic plans.
- Google OAuth honors `user_registration` for new accounts: existing and linkable accounts still sign in, but creating an account returns `404 feature.disabled` when registration is off for the tenant.
- Webhook signature verification compares the timestamp against window bounds instead of `abs(now - ts)`, which saturated/overflowed for timestamps ~292+ years away and let millisecond or far-future timestamps pass any tolerance; `VerifyRequest` also rejects non-positive or implausible (> year 9999) seconds.
- Webhook delivery: a worker shutdown before any response gives the claimed attempt back and releases the job without consuming its attempt (no dead-lettering on the final attempt); outcome writes are fenced by the claimed attempt so a stale worker cannot overwrite a newer attempt; for 24h after an endpoint secret rotation, older deliveries are co-signed with the snapshot and current secret (`v1=<old>,v1=<new>`), then signed with the current secret only.
- Jobs: a handler that returns nil after its job timeout is recorded as succeeded (with a warning) instead of failed and re-run.
- Files: admin delete now emits `file.deleted`, and user/admin restore emit the new `file.restored` event, through the transactional outbox. A missing blob behind existing metadata returns `404` (logged as an error) instead of `500`; storage drivers return `storage.ErrNotFound`.
- Durable event job idempotency keys include the event name: `event:<name>:<id>:<handler>:<tenant>`.
- Dropped unused webhook indexes `idx_webhook_deliveries_tenant_status_next` (with the dead `CountWebhookDeliveriesByStatus` query), `idx_webhook_delivery_attempts_tenant_created`, and `idx_webhook_inbound_events_tenant_received` from the baseline.
- Identity/API-key hardening: the tenant middleware and `tenant.CachedResolver` copy the `X-Tenant-Slug`/`?tenant=` value before retaining it (Fiber's zero-copy strings corrupted cache keys across keep-alive requests). The startup admin seed creates the admin in one INSERT and only warns (never errors, promotes, or restores) when `ADMIN_EMAIL` belongs to a demoted, banned, or deleted account. Changing another user's email returns `403 permission.denied` when the caller uses an API key or the target is an admin; handler and service share `NormalizeEmail`. A ban stamps `users.credentials_revoked_at` (kept by unban), and the API-key verifier rejects keys created at or before it. The Google OAuth callback is Tier 1: `auth.google_login` is recorded in the sign-in transaction with the signed-state tenant and the signed-in user (no route Guard). `POST /auth/forgot-password` only enqueues a sealed `password_reset.request` job for every address; the job does the lookup and commits the token with its `email.send` job. Login lockout is keyed per (tenant, email, client IP) with a 5x global per-(tenant, email) budget (locked sources do not consume it), `VerifyPassword` uses the same keys, and a lockout-cache failure returns `503 system.unavailable`. `LinkGoogleAccount` never replaces a different `google_id` (409). Schema: `idx_users_tenant_email_lower_banned` backs `GetBannedUserByEmail`; refresh/reset/verification tokens use composite `(tenant_id, user_id)` FKs (CASCADE) with the per-user unique key moved to `(tenant_id, user_id)` and redundant `user_id`-only indexes dropped; dead `ListDeletedUsers` removed.

### Changed
- `files` is the ADR 0002 reference module: the service layer works on its own types through ports (`FileRepository`, `Transactor.InTx` with tx-bound `Audit` and `Outbox`) and no longer imports sqlc, pgx, or DTOs; the repository maps rows; dependencies are passed to `files.Build` instead of post-build `Attach*` type assertions. All `files` entries left the layering allowlist.
- `DB_PASSWORD` has no code default any more (it was an orphan `postgres` value no sample config used); credentials always come from `.env`, Compose, or the Kubernetes Secret.
- `audit.Policy`'s zero `Mode` now means `ModeRequired`; best-effort guards must set `ModeBestEffort` explicitly.
- Lockout/rate-limit cache keys use a 128-bit hex SHA-256 of the normalized email instead of the raw address (`<prefix><hash>:tenant=N[:ip=…]`); existing counters reset on deploy.
- Expired refresh/password-reset/verification tokens are purged by `identity.tokens.purge_expired`, scheduled hourly by the identity `ScheduleProvider` (requires `SCHEDULER_ENABLED=true`): 1,000-row `SKIP LOCKED` batches, at most 1 minute per table per run, new index `idx_refresh_tokens_expires_at`. It is a deliberate cross-tenant maintenance query.
- The identity service is split into files by responsibility (registration, authentication/lockout, Google OAuth, profile, password, webhooks, tx helper); `config/` is split into one file per concern, and `Config.Validate` runs section validators in a fixed order and reports the first error.
- Makefile: `make build` writes `bin/server` and `bin/migrate` (`.exe` on Windows); `make migrate-up` is removed (use `make migrate-run`); `make migrate-down` requires `CONFIRM=yes` because it drops the whole schema; new `make test-race`; `swag`/`air` versions are pinned in the Makefile.
- `scripts/rename-module.sh` reads the current module path from `go list -m` and rewrites all tracked text files with perl (needs `go` and `perl`); there is no `OLD_MODULE` to edit.
- `test/arch`: `TestCodeSQLOnlyTouchesOwnedTables` also scans inline UPPERCASE SQL string literals in Go under `internal/modules/**` and `internal/platform/**` (platform tables owned by `platform/<pkg>`, `platform/features` the only reviewed exception) and rejects CTEs that shadow another owner's table; routes must place `authz.Require`/`audit.Guard` before the final handler and `Guard` after `Idempotency`; the baseline must be spelled exactly `000001`, and two spellings of one version are duplicates.
- The baseline schema changed again (new `users.credentials_revoked_at`, composite token FKs, audit DELETE/TRUNCATE triggers, index changes). There is still no deployed database, so `000001` was edited in place: recreate local databases created from an earlier baseline (`cmd/migrate` refuses to start and lists the missing objects).
- Dependency refresh: Compose `redis:8-alpine` (from 7; RDB data from 7 loads as-is) and `otel/opentelemetry-collector:0.162.0` (from 0.145.0); every action in `ci.yml` is pinned to a commit SHA like `release.yml`/`deploy.yml` (`actions/checkout` v7.0.1, `actions/setup-go` v7.0.0, `golangci-lint-action` v9.3.0).
- PostgreSQL 18 is the supported server: Compose (`postgres:18-alpine`, data volume renamed to `postgres18_data` and mounted at `/var/lib/postgresql` per the 18+ image layout), CI, and testcontainers. See `docs/operations.md` for moving a local 17 volume.
- **Breaking:** MinIO replaced by SeaweedFS. `STORAGE_DRIVER` accepts `local | s3 | seaweedfs`; `minio` now fails startup. The S3 client moved from minio-go to the official aws-sdk-go-v2 (`service/s3`); non-AWS endpoints use path-style addressing and request/response checksums only when required. `docker-compose.yml`'s `s3` profile runs `chrislusf/seaweedfs:4.48` (`weed server -s3`, port 8333) with the `STORAGE_S3_*` credentials; the default endpoint is `seaweedfs:8333`.
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
- Typed in-process event bus (`internal/platform/events`) with `Publish`, `PublishLocal`, and transaction-aware `PublishTx`: metadata carries event id/name/tenant/request/actor, local handlers run synchronously with panic isolation and `errors.Join`, and durable subscriptions enqueue one idempotent job per event and handler (`event:<name>:<id>:<handler>:<tenant>`). Adds `requestctx` request/actor propagation, `jobs.EnqueueOnce`/`EnqueueOnceTx`, optional `app.EventProvider`/`JobProvider`/`ScheduleProvider` hooks, and event/scheduler metrics.
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
