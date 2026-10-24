<div align="center">

![fiber-golang-boilerplate](https://socialify.git.ci/chuanghiduoc/fiber-golang-boilerplate/image?font=JetBrains+Mono&forks=1&issues=1&language=1&name=1&owner=1&pattern=Solid&pulls=1&stargazers=1&theme=Auto)

[![CI](https://github.com/chuanghiduoc/fiber-golang-boilerplate/actions/workflows/ci.yml/badge.svg)](https://github.com/chuanghiduoc/fiber-golang-boilerplate/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/chuanghiduoc/fiber-golang-boilerplate)](https://go.dev/)
[![License](https://img.shields.io/github/license/chuanghiduoc/fiber-golang-boilerplate)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/chuanghiduoc/fiber-golang-boilerplate)](https://goreportcard.com/report/github.com/chuanghiduoc/fiber-golang-boilerplate)

[![Fiber](https://img.shields.io/badge/Fiber-v3-00ACD7?logo=go&logoColor=white)](https://gofiber.io/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-17-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org/)
[![Redis](https://img.shields.io/badge/Redis-DC382D?logo=redis&logoColor=white)](https://redis.io/)
[![Docker](https://img.shields.io/badge/Docker-2496ED?logo=docker&logoColor=white)](https://www.docker.com/)
[![Swagger](https://img.shields.io/badge/Swagger-85EA2D?logo=swagger&logoColor=black)](https://swagger.io/)
[![Prometheus](https://img.shields.io/badge/Prometheus-E6522C?logo=prometheus&logoColor=white)](https://prometheus.io/)

</div>

Production-minded REST API boilerplate built with Go Fiber v3, PostgreSQL, and sqlc.

## Tech Stack

- **Framework**: [Fiber v3](https://docs.gofiber.io/) (Go 1.27.3+)
- **Database**: PostgreSQL 18 with [pgxpool](https://github.com/jackc/pgx)
- **Query**: [sqlc](https://sqlc.dev/) (type-safe SQL code generation)
- **Migration**: [golang-migrate](https://github.com/golang-migrate/migrate) via one `cmd/migrate` entrypoint (single init schema)
- **Auth**: JWT ([golang-jwt](https://github.com/golang-jwt/jwt)) + Google OAuth 2.0
- **Validation**: [go-playground/validator](https://github.com/go-playground/validator)
- **Logging**: slog (stdlib structured logging)
- **Docs**: Swagger/OpenAPI via [swaggo](https://github.com/swaggo/swag)
- **Linter**: [golangci-lint v2](https://golangci-lint.run/)
- **Cache**: In-memory or Redis
- **Storage**: Local filesystem, AWS S3, or SeaweedFS (S3 gateway)
- **Email**: SMTP or console (dev)
- **Metrics**: Prometheus
- **Tracing**: OpenTelemetry (OTLP/gRPC, disabled by default; see [docs/tracing.md](docs/tracing.md))
- **Container**: Docker + Docker Compose

## Architecture

The project is a **modular monolith**: one deployable, module-owned route tables, and
platform packages for cross-cutting mechanics.

```
cmd/api/main.go                     Infra setup, catalog registration, lifecycle, graceful shutdown
internal/bootstrap/                 Single ordered module list + contract cross-wiring
cmd/migrate/main.go                 The only DB initializer/migrator (single init schema)
cmd/seed/main.go                    Standalone admin seeder (schema must exist)
internal/app/                       Composition root: Fiber, global middleware, module registry
internal/modules/
  identity/                         Auth, users, sessions, tokens
                                    (dto + repository + service + handler + adapter
                                     + queries + generated sqlc + admin seed)
  files/                            Upload, download, soft delete, restore
                                    (dto + repository + service + handler + adapter
                                     + queries + generated sqlc)
  features/                         Feature-flag admin API
  notifications/                    In-app feed, channel preferences, email channel
                                    (dto + repository + service + handler
                                     + queries + generated sqlc)
  admin/                            Back-office facade
                                    (dto + service + handler, contracts-only)
  webhooks/                         Outbound endpoints/deliveries, inbound
                                    sources/events, public HMAC receiver
                                    (dto + repository + service + handler
                                     + queries + generated sqlc)
internal/contracts/                 Narrow cross-module identity/files admin,
                                    notification, and principal-reader
                                    interfaces plus webhook event/dispatcher
                                    types (stdlib only)
internal/platform/
  authz/                            Permission catalog + Fiber middleware
  audit/                            Audit event recorder + mutating-request middleware
  events/                           Typed in-process bus + durable job-backed subscriptions
  features/                         Compile-time flag catalog, evaluator, snapshot cache, DB store
  i18n/                             Embedded en/vi catalogs, Accept-Language negotiation, localization
  jobs/                             Durable PostgreSQL jobs, worker, leases, idempotent enqueue
  requestctx/                       Request ID + actor propagation through context
  scheduler/                        PostgreSQL-leader interval/cron scheduler and run ledger
  session/                          Token-version/session validation
  tenant/                           Tenant resolution (X-Tenant-Slug + auth query fallback)
  versioning/                       V1/V2 policy, Accept-Version assertions, deprecation headers
internal/platform/middleware/      JWT, rate limit, logger, recovery, security headers, metrics, idempotency
pkg/
  apperror/                         RFC 9457 errors + typed error catalog
  response/                         JSON response helpers
  database/                         Pool, TxManager, migration runner
  webhook/                          HMAC signing/verification + SSRF-safe client
  validator/ token/ cache/ storage/ email/ pagination/ logger/ health/ oauth/ metrics/ async/
migrations/000001_init_schema.*     Single consolidated init schema (up/down)
docs/                               Swagger + architecture/API/module/ops docs
deploy/k8s/                         Kubernetes production manifests (see deploy/k8s/README.md)
```

See [docs/architecture.md](docs/architecture.md) for dependency rules and the
strangler status, [docs/api-design.md](docs/api-design.md) for the API design
system (errors, pagination, idempotency, status codes),
[docs/module-template.md](docs/module-template.md) for the canonical module
layout and scaffold, [docs/operations.md](docs/operations.md) for the
operations runbook, and [docs/webhooks.md](docs/webhooks.md) for outbound
dispatch, signing, SSRF, retries, and the inbound receiver. Tracing
configuration, privacy rules, and the local collector live in
[docs/tracing.md](docs/tracing.md). Feature flags are documented in
[docs/feature-flags.md](docs/feature-flags.md).

Request flow: `Client → app middleware (request ID, locale) → /api/vN version guard → tenant middleware → module route → handler/service/repository → sqlc → PostgreSQL`.

**Schema ownership:** only `cmd/migrate` applies DDL. The API and seed commands
assume the schema exists, so replicas never race on migrations. Fresh installs
run the single `000001_init_schema.up.sql`; add new timestamped migrations
(`make migrate-create`) only after a database is deployed.

## Multi-tenancy

The schema, request pipeline, and identity module are tenant-scoped. One user
belongs to exactly one tenant, and no identity read or write crosses tenants.

- The baseline seeds a `default` tenant with id `1`. Requests without
  `X-Tenant-Slug` resolve to `TENANT_DEFAULT_SLUG` (`default`) in local/test
  compatibility mode. `TENANT_REQUIRE_HEADER=true` rejects header-less
  requests instead, and **production requires it** so a write can never
  silently fall back to tenant 1. Staging should set it too.
- `X-Tenant-Slug` selects the request tenant on `/api/v1` and `/api/v2` routes.
  Invalid, unknown, or suspended slugs return `404` `tenant not found`. Startup
  checks tenant *existence* separately: a suspended configured/default tenant
  does not crash `cmd/api` or `cmd/seed`; its requests return `404` until it is
  reactivated.
- Public auth calls operate on the resolved tenant: registration, login,
  forgot/reset password, resend/verify email, and refresh. A client that
  registered in a non-default tenant must send the same header on those calls.
  Browser navigation and emailed links cannot send a header, so
  `GET /auth/google` and the reset/verify endpoints also accept a validated
  `?tenant=<slug>` query parameter; an explicit `X-Tenant-Slug` always wins.
- Access tokens carry a `tenant_id` claim. Authenticated requests use the
  claim; if an explicit `X-Tenant-Slug` resolves to a different tenant, the
  request fails with `403 permission.denied` (`tenant mismatch`). Tokens with a
  zero or negative tenant claim are rejected with `401`.
- All identity SQL is scoped by `tenant_id`: users by id, email, Google ID,
  admin lists/stats, and refresh/reset/verification tokens. A cross-tenant id
  or token behaves as not found (admin/user APIs return `404`; refresh returns
  `401`; reset/verification link tokens return `400`). A request for an
  unknown or suspended tenant is rejected at resolution with `404`.
- Emails and Google IDs are unique per tenant. The last-admin database trigger
  and the application check protect each tenant independently.
- Identity and files are tenant-isolated: all user/admin/stats and
  refresh/reset/verification queries filter `tenant_id`; every file metadata
  read/write, admin list/stats, download, delete, and restore filters the
  request tenant. Cross-tenant ids behave as `404`. Uploaded blobs use
  `<tenantID>/<userID>/<uuid>.<ext>`.
- Durable jobs and audit events carry `tenant_id`: enqueue records the request
  tenant, the worker restores tenant context for handlers, and audit rows are
  tenant-scoped.
- Feature-flag targets carry `tenant_id` and a nullable `user_id`; the admin
  repository derives the tenant from context, and a composite foreign key
  rejects a target pointing at a user in another tenant. Flag defaults/kill
  switches are process-wide. See [Feature Flags](docs/feature-flags.md).
- Webhook endpoints, versioned signing secrets, deliveries, attempts, inbound
  sources, and inbound events are tenant-scoped in SQL and indexes. The public
  inbound receiver resolves the tenant from the source ID, verifies the source
  HMAC, and writes/audits under that tenant regardless of `X-Tenant-Slug`.
- Notifications and notification preferences are tenant- and user-scoped:
  every list/read/read-all/count/preference query filters both columns.
  Cross-tenant or other-user ids return `404`. Email delivery inherits the
  notification tenant in `jobs.tenant_id`.
- Google OAuth binds the tenant into the OAuth `state` (and state cookie). The
  callback ignores `X-Tenant-Slug`, uses the state tenant, and refuses an
  unknown or suspended tenant even in `TENANT_REQUIRE_HEADER=true` mode.
- Password-reset and verification links include `?tenant=<slug>` so the
  browser can complete the flow without a custom header. The token row itself
  is tenant-bound; completing it under another tenant returns `400`.
- Idempotency keys are scoped by method, path, tenant ID/slug, and caller
  (user ID, or IP for public routes), so the same key/body/IP in two tenants
  cannot replay one tenant's `201` into another.
- Audit is fail-closed for privileged mutations. Tier 1 services write the
  audit row in the mutation transaction (audit failure rolls back); Tier 2
  routes write a pre-write intent row through `audit.Guard` and complete it
  with the observed outcome. A required intent that cannot be stored returns
  `503 system.audit_unavailable` before the handler runs. Intent, completion,
  transaction-missing, and domain-missing failures are exported as Prometheus
  counters.
- `POST /auth/logout` revokes by possession of the opaque refresh token. A
  tenant-B token sent without `X-Tenant-Slug` is still deleted; logout returns
  `204` even when the token is already absent.
- Tokens issued before multi-tenancy have no `tenant_id` claim and are rejected
  fail-closed. Rotate access tokens after upgrading (users must log in again);
  refresh/reset/verification rows default to tenant 1.
- The session validator compares the JWT `tenant_id` with the stored user tenant
  and rejects suspended tenants within the 5-second session-cache TTL. A token
  minted while a tenant was active can therefore remain valid for that short
  window; unknown/suspended request tenants are rejected by middleware first.
- `tenant.EffectiveID(ctx)` falls back to tenant 1 only for jobs, internal
  callers, and tests without request context. HTTP writes must resolve a
  tenant through middleware (production enforces the header).
- **Remaining:** row-level security and per-tenant limits/quotas.

## Internationalization and API Versioning

**Locales.** `internal/platform/i18n` embeds English (`en`) and Vietnamese
(`vi`) JSON catalogs. Global middleware negotiates `Accept-Language` using
q-values, case-insensitive matching, region truncation (`vi-VN` → `vi`), and
wildcards, then falls back to `I18N_DEFAULT_LOCALE` (default `en`). Responses
carry `Content-Language` and `Vary: Accept-Language`. RFC 9457 error titles and
validation messages are localized; `AppError.DetailKey` opts a detail into
translation. Error `code`, `type`, `status`, and `instance` never change.

**API versions.** `/api/v1` is always mounted. A module can implement
`app.V2Module` (in addition to `app.Module`) to mount `/api/v2`; the v2 group is
created only when at least one module opts in (otherwise `/api/v2` returns
406). Clients may send `Accept-Version: 1|v1|2|v2`; it must match the path
version or the request fails with `406 api.version_unsupported`. Every
versioned response carries
`API-Version` and `Vary: Accept-Version`; a deprecated version additionally
emits `Deprecation`, `Sunset`, and `Link` headers from `versioning.Config`.
Any other `/api/vN` path returns 406 rather than a generic 404.

## Quick Start

### 1. Create your project

Click **"Use this template"** on GitHub, then:

```bash
git clone https://github.com/yourname/yourproject.git
cd yourproject
./scripts/rename-module.sh github.com/yourname/yourproject
go mod tidy
```

### 2. Run

**With Docker (recommended):**

```bash
cp .env.example .env
make docker-run
```

Services: API (:8080), PostgreSQL (:5432), Redis (:6379). Optional SeaweedFS S3 gateway: `STORAGE_DRIVER=seaweedfs docker compose --profile s3 up` (:8333).

**Without Docker** (requires Go 1.27.3+, PostgreSQL):

```bash
cp .env.example .env
# Edit .env with your database credentials
make run
```

`make run` applies the schema through `cmd/migrate` before starting the API.
Docker Compose runs a one-shot `migrate` service before the app; the API seeds
the admin user on startup.

## Kubernetes Deployment

Production-oriented manifests live in [`deploy/k8s`](deploy/k8s/README.md) with
staging and production overlays.
[`.github/workflows/release.yml`](.github/workflows/release.yml) builds and
pushes multi-arch images to GHCR on `v*` tags, and
[`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) is an optional,
environment-gated CD path. PostgreSQL and Redis are expected to be managed
services; no stateful database is bundled in the app Deployment.

The migration Job is deliberately outside every app overlay. Apply the
environment prereqs, apply and await the migration overlay, then apply the app
overlay; see the deploy README for the full ordered flow, placeholders, secret
creation, promotion, verification, and rollback.

## Database & Migrations

Schema changes are a separate one-shot step owned by `cmd/migrate`:

```bash
make migrate-run   # apply pending migrations (the only DDL entrypoint)
make run           # apply migrations, then start the API
make seed          # apply migrations, then seed the admin user
```

Docker Compose runs the one-shot `migrate` service before the `app` service
(`service_completed_successfully`). `cmd/api` and `cmd/seed` never apply DDL;
they assume the schema exists, so API replicas never race on migrations.

`cmd/migrate` loads only database configuration (`config.LoadDatabase`), so a
production migration job needs `DB_*` variables (host, port, database, user,
password, schema, SSL mode) and no JWT, admin, SMTP, or other application
secrets. `DB_SSLMODE=disable|allow|prefer` is rejected when `APP_ENV=production`. After
migrating, the command verifies the required tables, columns, unique and
tenant-leading indexes, and triggers, and fails with
an actionable error if it finds a legacy partial schema.

### Upgrading an existing database

`000001_init_schema` is a **fresh-install baseline**. It replaces the old
shipped migrations `000001`–`000003` and also creates the `jobs` and
`audit_events` tables.

A database that already applied the old migrations has a different
`schema_migrations` history (normally version 3 if all old migrations were
applied). Do not point it at the new `000001`: running the baseline over the
old schema is never supported.

This repository deliberately ships no sequential migrations after the
baseline: `test/arch/migrations_test.go` rejects any version other than `1`
that is not a UTC timestamp, so do not restore the old `000002`/`000003` files
into `migrations/` (a fresh install would then run them after the new
baseline). golang-migrate cannot advance a database whose recorded version is
missing from the source, so the supported upgrade is a one-time, reviewed
manual step:

1. Take a verified backup/PITR snapshot.
2. Apply the missing DDL listed in this section with `psql` in one
   transaction (tables, columns, indexes, triggers, and the default tenant),
   so the schema matches `000001_init_schema.up.sql`.
3. Record the baseline only after the schema matches it:
   `migrate -path migrations -database "$DSN" force 1`. This rewrites the
   version, not the schema; never use it to skip DDL that was not applied.
4. Run `cmd/migrate`. It applies any newer timestamped migrations and then
   verifies the required tables, columns, unique indexes, tenant-leading
   indexes, triggers, and default tenant, failing with the missing objects if
   step 2 was incomplete.

`cmd/migrate` prints this guidance when it detects a legacy version whose
source files are missing. Later migrations are ordinary timestamped files
(`make migrate-create`), which are always above the legacy versions.

The multi-tenant foundation also changes `000001` (new `tenants` table,
`tenant_id` columns, per-tenant indexes, and the per-tenant last-admin
trigger). An existing database does **not** get these objects from the baseline.
The manual upgrade DDL (step 2 above) must:

1. creates `tenants` and inserts `('default','Default')` as the first row so it
   receives id 1;
2. adds `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)` to
   `users`, `files`, `refresh_tokens`, `password_reset_tokens`,
   `email_verification_tokens`, `jobs`, and `audit_events`;
3. rebuilds the user indexes with `tenant_id` leading, then adds the
   tenant-leading indexes used by scoped queries;
4. replaces `protect_last_admin()` with the per-tenant version;
5. adds `feature_flags`, `feature_flag_targets`, their partial unique indexes,
   and a unique index on `users(tenant_id, id)` for tenant-safe user targets.

Keep the default tenant id 1 unless you also change the column defaults and
backfill every row. Fresh installs get all of this from `000001`.

Identity/files isolation needs no further schema change: the tenant columns and
indexes already exist. Existing access tokens without a `tenant_id` claim are
rejected with 401, so clients must log in again. Existing refresh/reset/
verification rows were backfilled to tenant 1 and remain usable only there.
New uploads use the blob key `<tenantID>/<userID>/<uuid>.<ext>`, while existing
rows keep their old `<userID>/<uuid>.<ext>` path; if you copy blobs, update
`files.storage_path` atomically.

Feature flags need the tables above on an existing database; without them the
evaluator still uses environment/compiled defaults, but the admin API fails
until the migration is applied and `cmd/migrate` verifies the objects.

Notifications add two new tenant-owned tables (`notifications` and
`notification_preferences`) plus their indexes. Existing databases need those
objects in the same manual upgrade DDL; fresh installs get them from `000001`.
`cmd/migrate` verifies both tables, their tenant columns, and the unique
preference index.

The upgrade DDL must also create every unique index an `ON CONFLICT` target
uses (`idx_jobs_tenant_idempotency_key`, `idx_scheduler_runs_schedule_slot`,
the feature-flag target indexes, and the webhook secret/delivery/attempt
indexes) and the `trigger_protect_last_admin`,
`trigger_audit_events_completion`, `trigger_audit_events_no_delete`, and
`trigger_audit_events_no_truncate` triggers; `cmd/migrate` reports any that
are missing or disabled. `audit_events` rejects `DELETE` and `TRUNCATE`; a
reviewed retention or erasure job must disable those triggers explicitly as
the table owner.

If a fresh schema was created out-of-band and already matches `000001`,
baseline it manually after verifying the objects (for example, set version 1
with the golang-migrate CLI's `force 1`) instead of running the file over
populated tables. Never edit a shipped migration after deployment.

## API Endpoints

In production (`TENANT_REQUIRE_HEADER=true`) every API request must select a
tenant explicitly with `X-Tenant-Slug`, except the documented browser-auth
paths that use `?tenant=` or the OAuth callback's signed `state`. Local/test
compatibility can omit both to use `TENANT_DEFAULT_SLUG`. Login,
registration, refresh, password reset, and email verification all resolve the
tenant from this header (or the default), so clients of a non-default tenant
must send it consistently. Authenticated requests must not send a tenant that
differs from the JWT `tenant_id` claim. Browser OAuth start and emailed
reset/verification links may instead carry a validated `?tenant=<slug>`;
`X-Tenant-Slug` takes precedence, and the OAuth callback uses the tenant bound
to its signed `state` (not the header). Reset/verification links include
`?tenant=<slug>` automatically.

### Auth (public)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/auth/register` | Register new user |
| POST | `/api/v1/auth/login` | Login, returns JWT + refresh token |
| POST | `/api/v1/auth/refresh` | Refresh access token |
| POST | `/api/v1/auth/logout` | Revoke one refresh token |
| POST | `/api/v1/auth/logout-all` | Revoke every refresh token for the current user |
| POST | `/api/v1/auth/forgot-password` | Request password reset email |
| POST | `/api/v1/auth/reset-password` | Reset password with token (`?tenant=` link fallback) |
| POST | `/api/v1/auth/verify-email` | Verify email with token (`?tenant=` link fallback) |
| POST | `/api/v1/auth/resend-verification` | Resend verification email |
| GET | `/api/v1/auth/google` | Google OAuth redirect (`?tenant=` browser fallback) |
| GET | `/api/v1/auth/google/callback` | Google OAuth callback (tenant from signed state) |

### Users (protected — JWT or `Authorization: ApiKey`)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/users/me` | Get current user |
| PUT | `/api/v1/users/me` | Update own profile |
| PUT | `/api/v1/users/me/password` | Change password |
| GET | `/api/v1/users/:id` | Get user by ID (self or admin) |
| GET | `/api/v1/users/` | List users (admin only) |
| PUT | `/api/v1/users/:id` | Update user (admin or self) |
| DELETE | `/api/v1/users/:id` | Delete user (admin or self) |

### Files (protected — JWT or `Authorization: ApiKey`)
All file routes are scoped to the request tenant; a file id from another tenant
returns `404`, including for admins. Blobs are stored under
`<tenantID>/<userID>/<uuid>.<ext>`.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/files/upload` | Upload file |
| GET | `/api/v1/files/` | List own files in the current tenant (paginated; `includeDeleted=true` shows trash) |
| GET | `/api/v1/files/:id` | Get tenant-owned file info |
| GET | `/api/v1/files/:id/download` | Download tenant-owned file (authenticated; storage is not publicly served) |
| DELETE | `/api/v1/files/:id` | Delete tenant-owned file (soft) |
| POST | `/api/v1/files/:id/restore` | Restore own soft-deleted file in the current tenant |

### Notifications (protected — JWT required)
Notifications are scoped to the authenticated user and request tenant.
`GET /notifications/` accepts `unreadOnly=true`; `POST /notifications/:id/read`
is idempotent and `POST /notifications/read-all` returns the number of newly
read rows.
Preferences default to enabled for both channels; only explicit rows are
returned. Reads require `notifications:read`, user mutations require
`notifications:write`, and sending to another user requires the admin-only
`notifications:send` permission. Email is a durable job.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/notifications/` | List own notifications (cursor; `unreadOnly`) |
| GET | `/api/v1/notifications/unread-count` | Unread badge count |
| GET | `/api/v1/notifications/:id` | Get one own notification |
| POST | `/api/v1/notifications/:id/read` | Mark one own notification read (idempotent) |
| POST | `/api/v1/notifications/read-all` | Mark all own notifications read |
| POST | `/api/v1/notifications/send` | Admin: notify a user in the tenant (`notifications:send`) |
| GET | `/api/v1/notification-preferences/` | List explicit channel preferences |
| PUT | `/api/v1/notification-preferences/` | Upsert `type` + `in_app`/`email` preference |

### Admin (protected — admin role; JWT or `Authorization: ApiKey`)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/admin/stats` | Statistics for the caller's tenant |
| GET | `/api/v1/admin/users` | List users in the caller's tenant (including deleted) |
| PUT | `/api/v1/admin/users/:id/role` | Update a user role in the caller's tenant |
| POST | `/api/v1/admin/users/:id/ban` | Ban a user in the caller's tenant (soft delete) |
| POST | `/api/v1/admin/users/:id/unban` | Unban a user in the caller's tenant (restore) |
| GET | `/api/v1/admin/files` | List files in the caller's tenant (including soft-deleted) |
| GET | `/api/v1/admin/files/:id/download` | Download any active file in the caller's tenant |
| DELETE | `/api/v1/admin/files/:id` | Soft-delete any file in the caller's tenant (takedown) |
| POST | `/api/v1/admin/files/:id/restore` | Restore any soft-deleted file in the caller's tenant |
| GET | `/api/v1/admin/features/flags` | List feature flags and effective values |
| GET | `/api/v1/admin/features/flags/:key` | Get a flag and current-tenant targets |
| PUT | `/api/v1/admin/features/flags/:key` | Upsert flag default, kill switch, description |
| DELETE | `/api/v1/admin/features/flags/:key` | Delete flag state (revert to env/compiled) |
| GET | `/api/v1/admin/features/flags/:key/targets` | List current-tenant targets |
| PUT | `/api/v1/admin/features/flags/:key/targets` | Upsert tenant-wide or user target |
| DELETE | `/api/v1/admin/features/flags/:key/targets/:id` | Delete a current-tenant target |

### Webhooks (protected — `webhooks:read` / `webhooks:write`; admin role)

All endpoint, delivery, and inbound-source routes are tenant-scoped. Secret
values are returned once on create/rotate and are never included in reads.
See [docs/webhooks.md](docs/webhooks.md) for signing, retry, SSRF, and receiver
details.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/webhooks/endpoints` | List outbound endpoints |
| POST | `/api/v1/webhooks/endpoints` | Create endpoint; returns a signing secret |
| GET | `/api/v1/webhooks/endpoints/:id` | Get endpoint |
| PUT | `/api/v1/webhooks/endpoints/:id` | Replace endpoint configuration |
| DELETE | `/api/v1/webhooks/endpoints/:id` | Soft-delete/disable endpoint |
| POST | `/api/v1/webhooks/endpoints/:id/rotate-secret` | Rotate signing secret |
| GET | `/api/v1/webhooks/deliveries` | List deliveries (`endpointId`, `status`) |
| GET | `/api/v1/webhooks/deliveries/:id` | Get delivery with payload snapshot |
| GET | `/api/v1/webhooks/deliveries/:id/attempts` | List delivery attempts |
| POST | `/api/v1/webhooks/deliveries/:id/replay` | Replay with the current URL/secret |
| GET | `/api/v1/webhooks/inbound-sources` | List inbound sources |
| POST | `/api/v1/webhooks/inbound-sources` | Create source; returns an HMAC secret |
| GET | `/api/v1/webhooks/inbound-sources/:id` | Get inbound source |
| PUT | `/api/v1/webhooks/inbound-sources/:id` | Update inbound source |
| DELETE | `/api/v1/webhooks/inbound-sources/:id` | Soft-delete/disable source |
| POST | `/api/v1/webhooks/inbound-sources/:id/rotate-secret` | Rotate HMAC secret |
| GET | `/api/v1/webhooks/inbound-sources/:id/events` | List deduplicated inbound events |
| POST | `/api/v1/webhooks/inbound/:id` | Public HMAC receiver (no JWT) |

### Notifications (protected — `notifications:read` / `notifications:write`)

All feed and preference routes are scoped to the authenticated user and tenant.
`POST /notifications/send` is admin-only (`notifications:send`). See
[docs/api-design.md](docs/api-design.md#notifications).

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/notifications` | List the caller's notifications (cursor, `unreadOnly`) |
| GET | `/api/v1/notifications/unread-count` | Unread badge count |
| GET | `/api/v1/notifications/:id` | Get one owned notification |
| POST | `/api/v1/notifications/:id/read` | Mark one notification read |
| POST | `/api/v1/notifications/read-all` | Mark every unread notification read |
| POST | `/api/v1/notifications/send` | Admin send to a user in the tenant |
| GET | `/api/v1/notification-preferences` | List explicit channel preferences |
| PUT | `/api/v1/notification-preferences` | Upsert `{type, channel, enabled}` |

### API keys (protected — `admin:api_keys:*`; admin role)

Plaintext keys are returned only on create/rotate; list/get responses never
contain them. The key inherits tenant/user binding and an explicit permission
scope subset.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/admin/api-keys` | List tenant-scoped API keys |
| POST | `/api/v1/admin/api-keys` | Create a key; returns the plaintext once |
| GET | `/api/v1/admin/api-keys/:id` | Get key metadata |
| PUT | `/api/v1/admin/api-keys/:id` | Update name/scopes |
| DELETE | `/api/v1/admin/api-keys/:id` | Revoke immediately |
| POST | `/api/v1/admin/api-keys/:id/revoke` | Revoke immediately (idempotent) |
| POST | `/api/v1/admin/api-keys/:id/rotate` | Rotate with a 24h grace window |

### Infrastructure
| Method | Path | Description |
|--------|------|-------------|
| GET | `/healthz` | Liveness probe |
| GET | `/readyz` | Readiness probe (DB + cache) |
| GET | `/metrics` | Prometheus metrics (requires `METRICS_AUTH_TOKEN` in production) |
| GET | `/swagger` | Swagger UI (non-production only) |

### Idempotency

Mutating endpoints accept an optional `Idempotency-Key` header (1–128
characters; only `[A-Za-z0-9._:-]`). When present:

- the first request runs and its 2xx status, body, and selected headers
  (`Content-Type`, `Location`, `ETag`) are cached for 24 hours; retries with the
  same key replay that response without running the handler again and carry
  `Idempotent-Replayed: true`
- for non-multipart request bodies up to 1 MiB, the key is bound to a SHA-256
  hash of the body. Reusing the key with a different body returns `409 Conflict`
  (`idempotency key was used with a different request body`) instead of replaying
  the first response. Multipart and larger bodies are not body-bound.
- the first request holds an owner-token lock for 2 minutes (above the 60-second
  HTTP write timeout). Concurrent requests with the same key while it is held
  return `409 Conflict`; after the owner finishes, retries replay the cached
  response. A lock is released only by its owner when the cache supports
  compare-and-delete.
- non-2xx responses are not cached, and cache failures fail open (the request
  runs normally)
- keys are scoped by method, path, tenant ID/slug, and caller (user ID, or IP
  for the public registration route), so the same key on another endpoint,
  user, or tenant does not collide or replay a cross-tenant `201`

Browser clients must be allowed to send `Idempotency-Key`. The default
`CORS_ALLOW_HEADERS` includes it along with `Accept-Language` and
`Accept-Version`; the default `CORS_EXPOSE_HEADERS` exposes `Location`, `ETag`,
`Idempotent-Replayed`, `Content-Language`, `API-Version`, `Deprecation`,
`Sunset`, and `Link`. If you override those settings, keep the relevant values.

This is opt-in and applied to registration, profile/password updates, user
updates/deletes, file upload/delete/restore, and admin role/ban/unban/file
actions. GET, HEAD, and OPTIONS requests never use it. Token-minting and
credential-verification endpoints (login, refresh, logout, logout-all,
forgot/reset/verify/resend, Google OAuth) are excluded; password change
(`PUT /users/me/password`) is covered because it is a normal authenticated
mutation, not a token-minting endpoint.

Registration is IP-scoped because no authenticated user exists yet. Two users
behind the same NAT should use distinct keys; reusing a key with a different
JSON body returns 409 rather than replaying another user's response. Cached 2xx
bodies may contain personal data and are retained for 24 hours, so size Redis
accordingly and account for eviction: evicted entries simply cause a later retry
to execute again. Use Redis (`CACHE_DRIVER=redis`) so replays and locks work
across multiple replicas; memory caching is per process.

## Makefile Commands

```bash
make build                        # Build server + migrate binaries
make run                          # Apply schema, then run locally
make migrate-run                  # Apply pending migrations via cmd/migrate
make test                         # Run unit tests
make test-integration             # Run integration tests (Docker/testcontainers, or set TEST_DATABASE_URL)
make lint                         # Run golangci-lint
make docker-run                   # Start with Docker Compose (includes migrate service)
make docker-down                  # Stop Docker Compose
make migrate-up                   # Run migrations with the golang-migrate CLI
make migrate-down                 # Rollback migrations
make migrate-create module=files name=add_x  # New UTC-timestamp migration (<ts>_<owner>_<purpose>)
make sqlc-generate                # Regenerate sqlc code
make swagger                      # Regenerate Swagger docs
make seed                         # Apply schema, then seed the admin user
make watch                        # Live reload with Air
```

`make migrate-run`, `make run`, and `make seed` use `cmd/migrate`.
`make migrate-up` and `make migrate-down` are the golang-migrate CLI equivalents.

## Background Jobs

`internal/platform/jobs` is a PostgreSQL-backed durable queue; email delivery
runs through it. Delivery is **at-least-once**, not exactly-once: a handler can
run again after a retry, panic, or lease expiry, so handlers must be idempotent
and safe to retry. Use `Queue.EnqueueTx` when a job must commit atomically with
the business transaction that produced it. Password-reset and email-verification
services already do this: the token row and the `email.send` job share one
transaction, so a crash cannot leave a usable link without a queued email.
When transactional email provisioning is wired (as in `identity.Build`),
registration is atomic as well: the user row, its verification token, and the
`email.send` job commit together, and an outbox write failure rolls the account
back (fail-closed). Without that wiring, the handler falls back to a
post-commit verification email. Resend-verification and email-change flows
still create their token and email in a separate transaction after the account
already exists.

- `Queue.EnqueueOnce` / `Queue.EnqueueOnceTx` accept a deterministic
  `Job.IdempotencyKey` and insert at most one job per `(tenant_id, key)`. They
  return the existing id and `created=false` for a duplicate, which makes
  producer retries and event fan-out safe. The key is a normal nullable column
  with a partial unique index; plain `Enqueue`/`EnqueueTx` do not suppress a
  duplicate key.
- Jobs are tenant-owned. `Queue.Enqueue` and `Queue.EnqueueTx` write
  `jobs.tenant_id` resolved from the context (`tenant.EffectiveID`, default
  tenant 1); an explicit positive `Job.TenantID` is kept. The claim query
  returns the column and the worker installs `tenant.WithID(ctx, tenantID)`
  before calling the handler, so email and audit handlers see the same tenant
  as the request that enqueued the work. Rows written before multi-tenancy
  default to tenant 1. Retention and Prometheus labels stay global
  (tenant-agnostic); use the `(tenant_id, status, run_at)` and
  `(tenant_id, kind, status)` indexes when inspecting per-tenant work.
- Enqueue also captures the active W3C `traceparent` in `jobs.trace_parent`.
  Each worker attempt starts a new-root `job.process` span with a link to that
  enqueue span, so `succeeded`, `retried`, and `dead` outcomes are independently
  inspectable. See [docs/tracing.md](docs/tracing.md).
- Claims use `FOR UPDATE SKIP LOCKED`; `attempts` is incremented at claim time.
  Failures retry with exponential backoff (1s doubling up to 1h) until
  `max_attempts` (default 10), then move to `dead`. An unregistered job kind is
  not special-cased: it retries with the same backoff and is dead-lettered only
  when its attempt budget is exhausted, so a rollout can register a handler
  before pending work dies.
- Claim failures are retried in-process with exponential backoff, starting at
  the poll interval and capped at 30s, so a transient database outage does not
  stop the worker (or the API that hosts it). `Run` returns only when its
  context is canceled.
- Each handler runs with a 10m timeout. Set it with `WithJobTimeout`; a value
  less than or equal to zero disables the deadline. A timed-out handler is
  canceled and recorded as a normal failure, so the job retries or is
  dead-lettered; one that ignores the cancellation and still returns nil is
  recorded as succeeded (its side effects already ran) with a warning log.
  Cancellation is cooperative: the worker cannot preempt a
  handler that ignores its context, so heartbeats keep running until that
  handler returns. The final bookkeeping write uses a detached 5s context.
- Running jobs renew a lease with a 30s heartbeat (5m lease by default). A
  `running` job whose lease expires is requeued by the next claim, so a crashed
  worker does not block it forever. Reclamation happens inside the claim query,
  so another worker must be polling to recover a dead worker's jobs; a
  reclaimed job consumes an attempt when claimed again. Heartbeat, success, and
  failure writes are fenced by `locked_by` and `attempts`, so the previous owner
  of a reclaimed job cannot renew or finish it. That owner can still be
  executing the old handler, so duplicate concurrent execution is possible.
  Tune the lease with `WithHeartbeatInterval` and `WithLeaseDuration`.
- `jobs` stores `run_at`, `locked_at`, `locked_by`, `attempts`, and
  `last_error`; use them to inspect retries and dead letters. Successful and
  dead jobs have their `payload` cleared, so reset/verification links from email
  jobs are not retained after delivery.
- Email job payloads are encrypted at rest. `cmd/api/main.go` derives a
  32-byte AES-256-GCM key from `JWT_SECRET` with HKDF-SHA256 (domain-separated
  for the `jobs/payload/v1` context) and passes a `jobs.PayloadCodec` to
  `NewQueuedSender` and `EmailHandler`. The stored `jobs.payload` is a versioned
  envelope (`v`, `nonce`, `data`) instead of readable JSON, so a database dump,
  backup, or replication stream during the queue window does not expose usable
  reset/verification links. Jobs queued as plaintext JSON before deployment are
  still delivered, but they remain readable at rest until delivered or cleared.
  External tooling that inspects `jobs.payload` sees the envelope, not email
  JSON. Encrypted jobs use the distinct kind `email.send.encrypted`; new
  workers register both `email.send` and `email.send.encrypted`. During a
  rolling deploy, old workers leave encrypted jobs queued as an unknown kind
  (retrying with backoff) instead of misreading the envelope, so no message is
  lost; they drain once the new workers are up. Draining before the rollout
  still gives immediate delivery. Rotating `JWT_SECRET` changes the derived key
  and makes already-queued encrypted jobs undecryptable; drain the queue before
  rotating the secret.
- A worker deletes terminal rows automatically: `succeeded` and `dead` jobs
  whose `updated_at` is older than 30 days are purged, by default every hour
  (the first pass runs as soon as the worker starts). Queued and running jobs
  are never deleted, regardless of age. Tune behavior with `WithRetention`
  (default `30*24h`; a value less than or equal to zero disables retention) and
  `WithPurgeInterval` (default `1h`; non-positive values are ignored). Each
  purge uses a detached 5s context and deletes at most 1,000 rows per batch, so
  a large backlog cannot hold one transaction open; failures are logged and
  retried on the next interval.
- Prometheus counters expose job lifecycle and audit health:
  `jobs_claimed_total{kind}`, `jobs_succeeded_total{kind}`,
  `jobs_retried_total{kind}`, `jobs_dead_total{kind}`,
  `jobs_claim_errors_total`, `jobs_purged_total`,
  `audit_write_failures_total`, `audit_intent_failures_total`,
  `audit_completion_failures_total`, `audit_tx_missing_total`, and
  `audit_domain_missing_total`. Outcome counters advance only after the
  corresponding database write succeeds. `jobs_dead_total` counts handler
  failures that exhaust their attempts; a job dead-lettered by lease expiry is
  visible as `status = 'dead'` in the table but is not counted by that
  handler-outcome metric.
- Webhook delivery uses the same durable queue under the
  `webhook.delivery` kind. A delivery row and its first job commit atomically;
  the worker sends one signed HTTP attempt and schedules the next attempt as a
  **new** job with webhook-specific exponential backoff or `Retry-After`.
  Permanent 3xx/4xx responses (except `408`, `425`, `429`) mark the delivery
  `dead` immediately. Delivery is at-least-once, so receivers must deduplicate
  on `X-Webhook-Delivery`; every attempt is retained in
  `webhook_delivery_attempts`. A separate hourly retention loop deletes terminal
  deliveries and old inbound events after `WEBHOOK_RETENTION_DAYS`.
- On SIGINT/SIGTERM the worker stops claiming, cancels running handlers, and
  the app waits up to 10s for them to drain. Handlers must honor context
  cancellation: the worker cannot preempt a handler that ignores it, heartbeats
  continue until that handler returns, and a non-cooperative handler can exceed
  the shutdown drain window.

## Scheduler and Events

`internal/platform/scheduler` is an optional global scheduler. It is disabled by
default; set `SCHEDULER_ENABLED=true` to start it in `cmd/api`. The scheduler is
not tenant-scoped: each schedule targets a tenant in its `Spec`, and the job
payload records the schedule name and slot.

- Every process competes for one row in `scheduler_leases`. The holder renews
  the lease every `SCHEDULER_HEARTBEAT_SECS`; a failed renewal steps down before
  the next pass so two leaders cannot both enqueue. A crashed holder is
  replaced after `SCHEDULER_LEASE_TTL_SECS`.
- `scheduler_runs` is a unique `(schedule, scheduled_for)` ledger. The ledger
  insert and the job enqueue share one transaction, so a crash or duplicate
  leader cannot enqueue the same slot twice. `scheduler_runs` is retained for
  `SCHEDULER_RETENTION_DAYS` and is purged by the leader.
- `Spec.Interval` schedules align to Unix epoch boundaries and must be a whole
  number of seconds. `Spec.Cron` accepts standard 5-field expressions (lists,
  ranges, steps, `0`/`7` Sunday) plus `@hourly`, `@daily`/`@midnight`,
  `@weekly`, `@monthly`, `@yearly`, and `@annually`, evaluated in UTC. Set
  exactly one of interval or cron.
- `SCHEDULER_CATCHUP` controls slots missed while no leader ran: `none` skips
  them and waits for the next boundary, `latest` enqueues only the newest,
  `all` replays every slot up to `SCHEDULER_CATCHUP_LIMIT` per pass. A schedule
  with no ledger history starts from the current slot; `all` cannot invent
  pre-history. `none` still runs during continuous leadership when a poll
  arrives late, so a schedule shorter than the poll interval is not starved.
- Due slots enqueue the configured job kind with payload
  `{"schedule":..., "scheduledFor":..., "data":...}` and idempotency key
  `scheduler:<name>:<slot>`. `cmd/api` logs the registered schedule count and
  stops the scheduler before draining the worker.
- Modules can implement `app.ScheduleProvider` (`RegisterSchedules`) to add
  schedules without importing another module.

`internal/platform/events` is a typed in-process bus with optional durable
delivery through the same jobs queue:

- `events.NewEvent(name, data)` builds an `Event` with a fresh UUID and UTC
  timestamp. `PublishLocal` runs subscribers synchronously; `Publish` also
  enqueues durable subscriptions; `PublishTx` uses the caller's transaction so
  the business row and durable fan-out commit or roll back together.
- Event metadata carries `id`, `name`, `tenantId`, `requestId`, `actorId`,
  `occurredAt`, and JSON `data`. Missing tenant/request/actor values are
  resolved from the context (`tenant.WithID`, request middleware, and JWT
  middleware); explicit values win. Durable handlers reinstall all three.
- `Subscribe` registers a local handler. `SubscribeDurable` registers one
  handler with a stable handler name and job kind (default
  `events.<event>.<handler>`); each event enqueues one job per durable handler
  with idempotency key `event:<id>:<handler>:<tenantId>`.
- Handler panics are recovered per handler and errors are combined with
  `errors.Join`; one failing subscriber never skips the others. Durable
  delivery is at-least-once, so durable handlers must be idempotent.
- `RegisterJobs` turns durable subscriptions into worker handlers. The app calls
  it after modules register, so a module only implements `app.EventProvider`
  (`RegisterEvents`) and `app.JobProvider` (`RegisterJobs`) when it needs them;
  all three optional provider hooks are nil-safe.
- Metrics: `events_published_total{event,mode}`,
  `events_handled_total{event,outcome}`, and
  `events_enqueue_errors_total{event}`. Scheduler metrics are
  `scheduler_runs_total{schedule,outcome}`, `scheduler_leader`,
  `scheduler_enqueue_errors_total{schedule}`, and
  `scheduler_runs_purged_total`.

## Adding a New Module

Use `scripts/new-module.sh <name>` to create the compile-safe layout, then
follow [docs/module-template.md](docs/module-template.md) for route wiring,
permissions, audit, idempotency, tables, contracts, tests, and the full
checklist.

1. Create `internal/modules/<name>/module.go` implementing `app.Module`
   (`Name`, `RegisterRoutes`). Implement `app.V2Module` (`RegisterV2Routes`)
   only when the module has a breaking v2 surface.
2. Keep the module's handler/service/repository/DTO inside its directory.
   Do not import another module's internals; use `internal/contracts` or a
   narrow exported interface.
3. Declare permissions, error codes, flags, and en/vi strings in the module's
   `catalog/` (self-registered through the app provider hooks) and protect
   every route with `authz.Require(...)`. The legacy exceptions are documented in the
   [module template](docs/module-template.md#legacy-exceptions-do-not-copy);
   do not copy them.
4. Write queries in the module-scoped
   `internal/modules/<name>/queries/<name>.sql` file (the scaffold appends the
   `sqlc.yaml` block) and run `make sqlc-generate`.
5. For a deployed database, `make migrate-create module=<name> name=<purpose>`. For this boilerplate's
   fresh installs, update the single `000001_init_schema` file.
6. Register the module once in `internal/bootstrap/modules.go`.
7. Add module unit tests and an integration test; the architecture test in
   `test/arch` rejects cross-module imports.
8. Run `make swagger` and `make check` before opening a PR.

> `identity`, `files`, and `admin` own all of their DTOs, repositories,
> services, and handlers under `internal/modules/<name>`. `admin` reaches
> identity/files only through the stdlib-only `internal/contracts` interfaces
> implemented by the owner modules' adapters. Generated sqlc code is
> module-scoped under `internal/modules/<name>/sqlc`; there is no shared
> generated package, and no module imports another module's sqlc types.

## Environment Variables

See [.env.example](.env.example) for all available configuration options with defaults.

Key settings:
- `APP_ENV` — `local` | `test` | `development` | `staging` | `production` (affects logging format and production-only safety checks)
- `APP_BODY_LIMIT` — must be greater than `STORAGE_MAX_FILE_SIZE` (multipart overhead)
- `DB_CONNECT_TIMEOUT` — database connect timeout in seconds (default 10)
- `REQUIRE_EMAIL_VERIFICATION` — require a verified email before login/refresh
- `TENANT_DEFAULT_SLUG` — tenant for header-less requests in compatibility mode (default `default`)
- `TENANT_REQUIRE_HEADER` — require `X-Tenant-Slug` on `/api/v1`/`/api/v2` requests (default `false`; **must be `true` in production**, recommended in staging)
- `I18N_DEFAULT_LOCALE` — fallback locale for `Accept-Language` (`en` or `vi`, default `en`; unsupported values fail startup)
- `FEATURE_FLAGS` — comma-separated `key=true|false` overrides; a bare key means true (unknown keys fail startup)
- `FEATURE_FLAGS_FORCE_OFF` — comma-separated keys that always evaluate false, even during a database outage
- `FEATURE_FLAGS_CACHE_TTL_SECONDS` / `FEATURE_FLAGS_MAX_STALENESS_SECONDS` — snapshot freshness and stale-serving bounds (defaults 30/300)
- `SCHEDULER_ENABLED` — run the optional PostgreSQL-leader scheduler (default `false`)
- `SCHEDULER_LEASE_TTL_SECS` / `SCHEDULER_HEARTBEAT_SECS` / `SCHEDULER_POLL_SECS` — leader lease, renewal, and evaluation timings (defaults 30/10/15)
- `SCHEDULER_CATCHUP` — `none` | `latest` | `all` (default `latest`); `SCHEDULER_CATCHUP_LIMIT` bounds `all` per pass
- `SCHEDULER_RETENTION_DAYS` — `scheduler_runs` retention (default 30)
- `STORAGE_DRIVER` — `local` | `s3` | `seaweedfs`
- `CACHE_DRIVER` — `memory` | `redis` (Redis shares rate-limit counters, login lockouts, session invalidation, and idempotency locks/replays across replicas)
- `EMAIL_DRIVER` — `console` | `smtp` (`console` is rejected in production)
- `DB_SSLMODE` — `disable`, `allow`, and `prefer` are rejected in production; use `require` or `verify-full`
- `METRICS_AUTH_TOKEN` — required in production; sent as `Authorization: Bearer <token>`
- `OTEL_ENABLED` — enable OpenTelemetry tracing (default `false`); `OTEL_TRACES_EXPORTER` is `otlp` or `none`, and `OTEL_EXPORTER_OTLP_ENDPOINT` defaults to `localhost:4317`
- `ADMIN_EMAIL` / `ADMIN_PASSWORD` — Auto-seed admin user on API startup (also available via `make seed`; password must be 12+ chars in production)
- `WEBHOOK_SECRET_ENCRYPTION_KEY` — AES-256-GCM key for stored webhook secrets; required in production (local/test fall back to `JWT_SECRET`)
- `WEBHOOK_*_TIMEOUT_SECS`, `WEBHOOK_RETRY_*_SECS`, `WEBHOOK_MAX_ATTEMPTS`, `WEBHOOK_RETENTION_DAYS`, `WEBHOOK_TIMESTAMP_TOLERANCE_SECS` — delivery/verification bounds
- `WEBHOOK_SSRF_ALLOWLIST` — comma-separated private IPs/CIDRs/hosts permitted as delivery targets; empty keeps SSRF protection on

## Pagination

List endpoints return `{object, url, data, hasMore, nextCursor}`. When
`hasMore` is true, pass `nextCursor` as the `startingAfter` query parameter on
the next request. `limit` is clamped to 100.

## Testing

```bash
make test                 # unit tests
make test-integration     # testcontainers (Docker)
# or, without Docker, against a disposable schema on an existing PostgreSQL:
TEST_DATABASE_URL='postgres://user:pass@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -tags=integration -race -count=1 ./...
```

Testcontainers pulls the Docker client as a test-only dependency. `govulncheck`
reports no called vulnerabilities, but GitHub may show open advisories for that
client until upstream ships a patched module; it is not linked into the API
binary.

## Scope and Remaining Work

This is a boilerplate, not a finished multi-tenant SaaS platform. The modular
monolith, durable jobs, audit trail, permission-based authorization, single
migration entrypoint, and tenant foundation (schema, resolution, JWT claim,
config) exist. Remaining work:

- Tenant isolation is implemented for identity, files, webhooks, and
  notifications, including tenant-scoped SQL/indexes, admin lists/stats,
  tenant-bound Google OAuth state, tenant-aware sessions, tenant-prefixed
  storage keys, source-resolved inbound webhook tenants, and tenant/user-scoped
  notification feeds and preferences. Row-level security and per-tenant limits
  remain future work.
- Notifications include an in-app feed, per-type channel preferences, and a
  durable email channel committed with the in-app row. Delivery remains
  at-least-once; channel preferences default to enabled when no row exists.
- Distributed tracing is available (HTTP, sanitized pgx queries, and durable
  jobs) but disabled by default; enable it per environment with
  `OTEL_ENABLED=true` and a collector endpoint. Environment promotion/rollout
  automation: metrics, Kubernetes manifests (`deploy/k8s`), the GHCR release
  workflow, and an optional gated CD workflow exist.

All bounded-context internals are module-owned under `internal/modules`;
identity, files, and webhooks own their `queries/` and generated `sqlc/`
packages, and `admin` composes through `internal/contracts`. No shared generated
package and no legacy handler/service/DTO/repository packages remain.

## Security notes

- Refresh tokens are hashed, atomic/single-use, bound to the user's token
  version, and rotated on every refresh.
- Access tokens carry `token_version`; password changes/resets, bans,
  unbans, and role changes invalidate them immediately.
- Access tokens also carry `tenant_id`. A request whose explicit
  `X-Tenant-Slug` resolves to a different tenant is rejected with `403`; a token
  without a tenant claim (or with a zero/negative claim) is rejected with
  `401`. A request for an unknown or suspended tenant is rejected by tenant
  resolution with `404` before authentication; a session snapshot that still
  says suspended is rejected with `401` until the 5-second cache TTL expires.
- Access tokens are validated against the current user record through a
  generation-keyed session cache. Use Redis in production so invalidation is
  shared across replicas.
- Emails are normalized and unique case-insensitively among active accounts
  within a tenant; email changes require the current password, reset
  verification, revoke sessions, and send a fresh verification link.
- Uploaded files are never served from a public directory; downloads require
  authentication and ownership (or admin role). File metadata and blob keys are
  tenant-scoped, and another tenant's id returns 404.
- Outbound webhook targets are SSRF-checked at dial time: private, loopback,
  link-local, multicast, CGNAT, and reserved ranges are blocked unless the exact
  host/IP/CIDR is in `WEBHOOK_SSRF_ALLOWLIST`; DNS is resolved once and the
  connection is pinned to the vetted IP; environment proxies and redirects are
  disabled. Delivery signing uses HMAC-SHA256 with a per-endpoint secret and a
  timestamp; receivers must deduplicate on `X-Webhook-Delivery`.
- Webhook secrets are AES-256-GCM encrypted at rest with a dedicated key
  (`WEBHOOK_SECRET_ENCRYPTION_KEY`, required in production). Plaintext is
  returned only on create/rotate. Rotating the encryption key requires rotating
  every outbound and inbound webhook secret.
- Banned emails cannot be reused through registration or Google login;
  self-deleted accounts can still be re-registered.
- `POST /auth/logout` revokes one refresh token by possession, even if the
  caller omits `X-Tenant-Slug`; use `/auth/logout-all` to bump the token version
  and invalidate access tokens immediately.
- Idempotency keys include the tenant ID and slug in their cache scope, so the
  same client key/body from one IP cannot replay across tenants. Existing
  cached replays expire naturally after their 24-hour TTL.
- Production startup rejects the sample JWT secret/default admin password,
  console email, wildcard CORS, insecure frontend/OAuth URLs, missing metrics
  auth, `DB_SSLMODE=disable`, `TENANT_REQUIRE_HEADER=false`, and a body limit
  smaller than the upload size.
