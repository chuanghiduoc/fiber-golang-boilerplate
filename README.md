<div align="center">

![fiber-golang-boilerplate](https://socialify.git.ci/chuanghiduoc/fiber-golang-boilerplate/image?font=JetBrains+Mono&forks=1&issues=1&language=1&name=1&owner=1&pattern=Solid&pulls=1&stargazers=1&theme=Auto)

[![CI](https://github.com/chuanghiduoc/fiber-golang-boilerplate/actions/workflows/ci.yml/badge.svg)](https://github.com/chuanghiduoc/fiber-golang-boilerplate/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/chuanghiduoc/fiber-golang-boilerplate)](https://go.dev/)
[![License](https://img.shields.io/github/license/chuanghiduoc/fiber-golang-boilerplate)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/chuanghiduoc/fiber-golang-boilerplate)](https://goreportcard.com/report/github.com/chuanghiduoc/fiber-golang-boilerplate)

[![Fiber](https://img.shields.io/badge/Fiber-v3-00ACD7?logo=go&logoColor=white)](https://gofiber.io/)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-18-4169E1?logo=postgresql&logoColor=white)](https://www.postgresql.org/)
[![Redis](https://img.shields.io/badge/Redis-8-DC382D?logo=redis&logoColor=white)](https://redis.io/)
[![Docker](https://img.shields.io/badge/Docker-2496ED?logo=docker&logoColor=white)](https://www.docker.com/)
[![Swagger](https://img.shields.io/badge/Swagger-85EA2D?logo=swagger&logoColor=black)](https://swagger.io/)
[![Prometheus](https://img.shields.io/badge/Prometheus-E6522C?logo=prometheus&logoColor=white)](https://prometheus.io/)

</div>

Production-minded REST API boilerplate built with Go Fiber v3, PostgreSQL, and sqlc.

## Tech Stack

| Concern | Choice |
|---------|--------|
| Framework | [Fiber v3](https://docs.gofiber.io/) (Go 1.27.3+) |
| Database | PostgreSQL 18 with [pgxpool](https://github.com/jackc/pgx) |
| Queries | [sqlc](https://sqlc.dev/) (type-safe SQL code generation) |
| Migrations | [golang-migrate](https://github.com/golang-migrate/migrate) through one `cmd/migrate` entrypoint (single init schema) |
| Auth | JWT ([golang-jwt](https://github.com/golang-jwt/jwt)), Google OAuth 2.0, `Authorization: ApiKey` keys |
| Validation | [go-playground/validator](https://github.com/go-playground/validator) |
| Logging | `slog` (stdlib structured logging) |
| API docs | Swagger/OpenAPI via [swaggo](https://github.com/swaggo/swag) |
| Lint | [golangci-lint v2](https://golangci-lint.run/) |
| Cache | In-memory or Redis |
| Storage | Local filesystem, AWS S3, or SeaweedFS (S3 gateway) |
| Email | SMTP or console (dev) |
| Metrics / tracing | Prometheus; OpenTelemetry OTLP/gRPC, disabled by default ([docs/tracing.md](docs/tracing.md)) |
| Container | Docker + Docker Compose; Kubernetes manifests in [`deploy/k8s`](deploy/k8s/README.md) |

## Architecture

A **modular monolith**: one deployable, module-owned route tables, and platform
packages for cross-cutting mechanics. By default (`APP_ROLE=all`) the job
worker and the optional scheduler run inside the API process; `APP_ROLE=api`
and `APP_ROLE=worker` split them into separate processes from the same image.
Schema changes run as a separate one-shot `cmd/migrate` step.

```mermaid
flowchart LR
    client([Client]) --> mw
    subgraph api["cmd/api process"]
        mw["Global middleware<br/>recovery, CORS, security headers,<br/>request ID, i18n, tracing, metrics,<br/>logger, body limit, audit, timeout"]
        ver["/api/vN<br/>version guard + tenant resolution"]
        mods["Modules<br/>identity, files, admin, features,<br/>webhooks, apikeys, notifications"]
        contracts["internal/contracts<br/>(stdlib-only interfaces)"]
        plat["internal/platform + pkg<br/>authz, audit, events, jobs,<br/>session, tenant, features, ..."]
        worker["Job worker<br/>+ optional scheduler"]
        mw --> ver --> mods
        mods --> contracts
        mods --> plat
        worker --> plat
    end
    plat --> pg[(PostgreSQL)]
    plat --> cache[(Redis or memory cache)]
    plat --> store[(Local, S3, or SeaweedFS)]
    worker --> mail[SMTP / console email]
    worker --> hooks[Webhook receivers]
    api -. OTLP, optional .-> otel[OTel collector]
    migrate["cmd/migrate<br/>(only DDL entrypoint)"] --> pg
```

| Path | Responsibility |
|------|----------------|
| `cmd/api` | Infra setup, catalog registration, lifecycle, graceful shutdown |
| `cmd/migrate` | The only database initializer/migrator |
| `cmd/seed` | Standalone admin seeder (schema must exist) |
| `internal/bootstrap` | The single ordered module list and contract cross-wiring |
| `internal/app` | Composition root: Fiber app, global middleware, module registry, provider hooks |
| `internal/modules/identity` | Auth, global users, memberships, invitations, sessions, tokens, tenant lifecycle, admin seed |
| `internal/modules/files` | Upload, download, soft delete, restore |
| `internal/modules/admin` | Back-office facade over identity/files contracts (no SQL) |
| `internal/modules/features` | Feature-flag admin API |
| `internal/modules/webhooks` | Outbound endpoints/deliveries, inbound sources/events, public Standard Webhooks receiver, durable subscriber of domain events |
| `internal/modules/apikeys` | Tenant-scoped API keys with permission scopes |
| `internal/modules/notifications` | In-app feed, channel preferences, email channel |
| `internal/contracts/<provider>` | Stdlib-only cross-module interfaces, one package per providing module |
| `internal/platform` | `authz`, `audit`, `events`, `features`, `i18n`, `jobs`, `requestctx`, `scheduler`, `session`, `tenant`, `tracing`, `versioning`, `middleware` (JWT, rate limit, logger, recovery, security headers, metrics, idempotency) |
| `pkg` | `apperror` (RFC 9457 + catalog), `response`, `database` (pool, TxManager, migrations), `webhook` (HMAC + SSRF-safe client), `validator`, `token`, `cache`, `storage`, `email`, `pagination`, `logger`, `health`, `oauth`, `metrics`, `async` |
| `migrations` | One init pair `20260101000000_platform_init_schema` with `-- owner:` sections (ADR 0012); later migrations are `<UTC ts>_<owner>_<purpose>` |
| `deploy/k8s` | Kubernetes production manifests |

Every module follows the same layered slice (`handler → service → repository →
sqlc → PostgreSQL`) and owns its own `queries/` and generated `sqlc/` package.
Core packages and module services/repositories are Fiber-free; Fiber adapters
live in `<pkg>http` subpackages (`apperrorhttp`, `authzhttp`, `tenanthttp`,
`audithttp`, `featureshttp`, `apikeyhttp`), and `test/arch` enforces both that
split and the [ADR 0002](docs/adr/0002-module-layering.md) layering ratchet.

### Documentation map

| Doc | Covers |
|-----|--------|
| [docs/architecture.md](docs/architecture.md) | Module map, dependency rules, request lifecycle, cross-module communication |
| [docs/data-model.md](docs/data-model.md) | Core schema ER diagram and table ownership |
| [docs/api-design.md](docs/api-design.md) | Errors, pagination, auth, idempotency, status codes, compatibility |
| [docs/module-template.md](docs/module-template.md) | Canonical module layout and scaffold |
| [docs/operations.md](docs/operations.md) | Health, metrics, jobs, scheduler, shutdown, migrations, incidents |
| [docs/webhooks.md](docs/webhooks.md) | Outbound dispatch, signing, SSRF, retries, inbound receiver |
| [docs/feature-flags.md](docs/feature-flags.md) | Flag catalog, precedence, cache, admin API |
| [docs/tracing.md](docs/tracing.md) | OpenTelemetry configuration, privacy rules, collector |
| [deploy/k8s/README.md](deploy/k8s/README.md) | Kubernetes deployment flow |
| [docs/adr/README.md](docs/adr/README.md) | Architecture Decision Records |

**Schema ownership:** only `cmd/migrate` applies DDL. The API and seed commands
assume the schema exists, so replicas never race on migrations. Fresh installs
run the single init migration; add timestamped migrations
(`make migrate-create`) only after a database is deployed.

## Quick Start

```mermaid
flowchart TD
    A["Use this template on GitHub"] --> B["git clone, then<br/>scripts/rename-module.sh"]
    B --> C["go mod tidy<br/>cp .env.example .env"]
    C --> D{Docker?}
    D -->|yes| E["make docker-run"]
    D -->|no| F["make run<br/>(Go 1.27.3+, PostgreSQL)"]
    E --> G["Compose migrate service<br/>applies the schema"]
    F --> H["cmd/migrate<br/>applies the schema"]
    G --> I["API starts and seeds<br/>the admin user"]
    H --> I
    I --> J["Check /healthz, /readyz, /swagger"]
```

### 1. Create your project

Click **"Use this template"** on GitHub, then:

```bash
git clone https://github.com/yourname/yourproject.git
cd yourproject
./scripts/rename-module.sh github.com/yourname/yourproject
go mod tidy
```

The script reads the current module path from `go.mod` (`go list -m`) and
rewrites every tracked text file that mentions it with `perl` (Linux/macOS;
needs `go` and `perl`), so it is safe to re-run after a rename.

### 2. Run

**With Docker (recommended):**

```bash
cp .env.example .env
make docker-run
```

| Service | Port | Notes |
|---------|------|-------|
| API | `:8080` | Starts after the one-shot `migrate` service succeeds |
| PostgreSQL | `:5432` | `postgres:18-alpine` |
| Redis | `:6379` | `redis:8-alpine` |
| SeaweedFS S3 gateway | `:8333` | Optional: `STORAGE_DRIVER=seaweedfs docker compose --profile s3 up` |
| OTel collector | `:4317` | Optional `otel` profile; see [docs/tracing.md](docs/tracing.md) |

**Without Docker** (requires Go 1.27.3+ and PostgreSQL):

```bash
cp .env.example .env
# Edit .env with your database credentials
make run
```

`make run` applies the schema through `cmd/migrate` before starting the API.
In both paths the API seeds the admin user (`ADMIN_EMAIL`/`ADMIN_PASSWORD`)
on startup.

## Multi-tenancy

The schema, request pipeline, and every module are tenant-scoped. Users are
global identities (one email, one password, an optional linked Google
identity) that join tenants through **memberships** (ADR 0004); each
membership has its own role and status, and every tenant-scoped row of other
modules references the membership, not the user. A user joins another tenant
only by accepting an invitation.

```mermaid
flowchart TD
    R["/api/v1 or /api/v2 request"] --> P{OAuth callback?}
    P -->|yes| S["Tenant from signed OAuth state"]
    P -->|no| H{X-Tenant-Slug header?}
    H -->|yes| T["Resolve header slug"]
    H -->|no| Q{"Browser auth path with<br/>?tenant= query?"}
    Q -->|yes| T2["Resolve query slug"]
    Q -->|no| D{TENANT_REQUIRE_HEADER?}
    D -->|true| NF["404 tenant not found"]
    D -->|false| T3["Resolve TENANT_DEFAULT_SLUG"]
    T --> A{Active tenant?}
    T2 --> A
    T3 --> A
    A -->|no| NF
    A -->|yes| J{"Authenticated?"}
    J -->|no| OK["Handler runs in tenant"]
    J -->|yes| C{"Token tenant_id matches<br/>explicit tenant?"}
    C -->|no| F["403 permission.denied (tenant mismatch)"]
    C -->|yes| OK
```

| Area | Rule |
|------|------|
| Default tenant | The baseline seeds `default` with id `1`. Header-less requests resolve to `TENANT_DEFAULT_SLUG` in local/test compatibility mode. `TENANT_REQUIRE_HEADER=true` rejects them; **production requires it** (staging should set it too) so a write never silently falls back to tenant 1. |
| Resolution | `X-Tenant-Slug` selects the tenant on `/api/v1` and `/api/v2`. Invalid, unknown, or suspended slugs return `404 tenant not found`. |
| Startup | Startup checks tenant *existence* separately: a suspended configured/default tenant does not crash `cmd/api` or `cmd/seed`; its requests return `404` until reactivated. |
| Public auth | Register, login, and refresh use the resolved tenant; clients of a non-default tenant must send the header consistently. Login needs the global password **and** an active membership in that tenant. Registration creates the account and its membership there; an email that already has an account (in any tenant) gets `409 user.email_taken`. Password reset and email verification are per user. `GET /auth/google` and the reset/verify endpoints also accept a validated `?tenant=<slug>`; an explicit header always wins. Reset, verification, and invitation links include `?tenant=<slug>` so the frontend knows where to sign in. |
| Google OAuth | The tenant is bound into the OAuth `state` (and state cookie). The callback ignores `X-Tenant-Slug`, uses the state tenant, and refuses an unknown or suspended tenant even when the header is required. Linking to an existing account needs a verified local email and Google's `verified_email`; an existing user must already be a member of the state tenant (Google sign-in never joins a tenant implicitly). |
| Access tokens | Carry `tenant_id` and `sid` (the session). A conflicting explicit `X-Tenant-Slug` returns `403 permission.denied` (`tenant mismatch`); a zero, negative, or missing `tenant_id` or `sid` returns `401`, so tokens minted before sessions existed are rejected fail-closed and users must log in again after upgrading. |
| Sessions | Each sign-in is a session (the refresh-token family). The session validator checks the session is not revoked, the membership and user are active, the tenant is operational, and role/token version match, within the 5-second session-cache TTL. `GET /auth/sessions` lists and `DELETE /auth/sessions/:id` revokes the caller's sessions in the tenant; a reused refresh token revokes its whole session. |
| Identity | Membership, session, refresh-token, invitation, member-list, and stats SQL is scoped by `tenant_id`. Non-member user ids return `404`, another tenant's refresh token `401`. Emails and Google subjects are unique among live accounts globally; the last-admin trigger (on `memberships`) and application check protect each tenant independently. Ban, unban, role change, and removal are membership changes in one tenant. |
| Logout | `POST /auth/logout` revokes the session of the opaque refresh token by possession, even without `X-Tenant-Slug`, and returns `204` when the token is already absent. `POST /auth/logout-all` revokes every session in every tenant. |
| Files | Every metadata read/write, admin list/stats, download, delete, and restore filters the request tenant; cross-tenant ids return `404`. Blobs use `<tenantID>/<userID>/<uuid>.<ext>`. |
| Jobs and audit | Enqueue records the request tenant, the worker restores tenant context for handlers, and audit rows are tenant-scoped. |
| Feature flags | Targets carry `tenant_id` and a nullable `user_id`; a composite foreign key to `memberships` rejects a target pointing at a user who is not a member of that tenant. Defaults and kill switches are process-wide. See [docs/feature-flags.md](docs/feature-flags.md). |
| Webhooks | Endpoints, versioned secrets, deliveries, attempts, inbound sources, and inbound events are tenant-scoped in SQL and indexes. The public receiver resolves the tenant from the source id and writes/audits under it regardless of `X-Tenant-Slug`. |
| Notifications | Feed and preference queries filter both `tenant_id` and `user_id`; cross-tenant or other-user ids return `404`. Email jobs inherit the notification tenant. |
| Idempotency | Keys are scoped by method, path, tenant ID/slug, and caller (user, or IP for public routes), so a key/body/IP in two tenants cannot replay one tenant's `201` into another. |
| Audit | Fail-closed for privileged mutations: Tier 1 services write the audit row in the mutation transaction; Tier 2 routes write a pre-write intent row through `audithttp.Guard`. A required intent that cannot be stored returns `503 system.audit_unavailable` before the handler runs. |
| Internal fallback | `tenant.EffectiveID(ctx)` falls back to tenant 1 only for jobs, internal callers, and tests. HTTP writes must resolve a tenant through middleware. |
| Remaining | Row-level security and per-tenant limits/quotas. |

## Internationalization and API Versioning

**Locales.** `internal/platform/i18n` embeds English (`en`) and Vietnamese
(`vi`) catalogs. Middleware negotiates `Accept-Language` (q-values,
case-insensitive, region truncation `vi-VN` → `vi`, wildcards) and falls back
to `I18N_DEFAULT_LOCALE` (default `en`). Responses carry `Content-Language`
and `Vary: Accept-Language`. RFC 9457 titles and validation messages are
localized; `AppError.DetailKey` opts a detail in. `code`, `type`, `status`,
and `instance` never change.

**API versions.**

| Rule | Behavior |
|------|----------|
| Mounting | `/api/v1` is always mounted. A module implementing `app.V2Module` adds `/api/v2`; the v2 group exists only when at least one module opts in. |
| `Accept-Version` | Optional `1`, `v1`, `2`, or `v2`; must match the path version, otherwise `406 api.version_unsupported`. |
| Unknown version | Any other `/api/vN` path returns `406`, not `404`. |
| Headers | Every versioned response carries `API-Version` and `Vary: Accept-Version`; a deprecated version adds `Deprecation`, `Sunset`, and `Link` from `versioning.Config`. |

## Kubernetes Deployment

Production-oriented manifests live in [`deploy/k8s`](deploy/k8s/README.md) with
staging and production overlays.
[`.github/workflows/release.yml`](.github/workflows/release.yml) builds and
pushes multi-arch images to GHCR on `v*` tags, and
[`.github/workflows/deploy.yml`](.github/workflows/deploy.yml) is an optional,
environment-gated CD path. PostgreSQL and Redis are expected to be managed
services.

The migration Job is deliberately outside every app overlay: apply the
environment prereqs, apply and await the migration overlay, then apply the app
overlay. The deploy README covers placeholders, secrets, promotion,
verification, rollback, and the optional `base/worker` component that splits
job workers into their own Deployment (`APP_ROLE=worker`). Release images are
signed keyless with cosign, and the deploy workflow verifies the signature
before rollout when its image preflight is enabled.

## Database & Migrations

```bash
make migrate-run   # apply pending migrations (the only DDL entrypoint)
make run           # apply migrations, then start the API
make seed          # apply migrations, then seed the admin user
```

Docker Compose runs the one-shot `migrate` service before `app`
(`service_completed_successfully`). `cmd/api` and `cmd/seed` never apply DDL.

`cmd/migrate` loads only database configuration (`config.LoadDatabase`), so a
production migration job needs `DB_*` variables (host, port, database, user,
password, schema, SSL mode) and no JWT, admin, SMTP, or other application
secrets. `DB_SSLMODE=disable|allow|prefer` is rejected when
`APP_ENV=production`. After migrating, it verifies the required tables,
columns, unique and tenant-leading indexes, and triggers, and fails with an
actionable error on a legacy partial schema.

### Upgrading an existing database

The init migration (`20260101000000`, ADR 0012; see
[Data Model](docs/data-model.md)) is a **fresh-install baseline**. It replaces
the old shipped migrations `000001`–`000003` and the former consolidated
`000001_init_schema`. A database that applied any of those (recorded at a
sequential version) must never be pointed at it.

The repository ships no sequential migrations at all:
`test/arch/migrations_test.go` rejects any version that is not a UTC
timestamp, so do not restore the old `000001`–`000003` files. golang-migrate cannot advance a
database whose recorded version is missing from the source, so the supported
upgrade is a one-time, reviewed manual step:

```mermaid
flowchart LR
    A["1. Verified backup<br/>or PITR snapshot"] --> B["2. Apply missing DDL<br/>with psql in one transaction"]
    B --> C["3. migrate force 20260101000000<br/>(only after schema matches)"]
    C --> D["4. Run cmd/migrate<br/>(newer migrations + verification)"]
    D -->|missing objects| B
```

1. Take a verified backup/PITR snapshot.
2. Apply the missing DDL with `psql` in one transaction (tables, columns,
   indexes, triggers, and the default tenant) so the schema matches
   `migrations/20260101000000_platform_init_schema.up.sql`.
3. Record the baseline: `migrate -path migrations -database "$DSN" force 20260101000000`.
   This rewrites the version, not the schema; never use it to skip DDL that
   was not applied.
4. Run `cmd/migrate`. It applies newer timestamped migrations, then verifies
   tables, columns, unique and tenant-leading indexes, triggers, and the
   default tenant, listing anything step 2 missed.

`cmd/migrate` prints this guidance when it detects a legacy version whose
source files are missing. Later migrations are ordinary timestamped files
(`make migrate-create`), always above the legacy versions.

> **Identity model change (ADR 0004).** The baseline identity schema stores
> global `users` plus `memberships`, `user_passwords`, `user_identities`,
> `sessions`, and per-user reset/verification tokens, while legacy databases
> have per-tenant `users` rows with `tenant_id`, `role`, and the password on
> the row. Converting them is a **data migration**, not DDL alone: merge
> accounts that share a normalized email across tenants into one user, move
> each old row's tenant/role/ban into a membership, move passwords and Google
> IDs into their own tables, repoint every cross-module `(tenant_id, user_id)`
> FK at `memberships`, and drop refresh/reset/verification tokens (users sign
> in again). The repository ships no script for it; write and rehearse one
> against a restored backup. The table below still describes the non-identity
> objects.

The manual upgrade DDL (step 2) must cover:

| Object group | Required DDL |
|--------------|--------------|
| Tenants | Create `tenants` and insert `('default','Default')` first so it receives id 1. Keep id 1 unless you also change every column default and backfill. |
| Tenant columns | `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)` on `users`, `files`, `refresh_tokens`, `password_reset_tokens`, `email_verification_tokens`, `jobs`, and `audit_events`. |
| Indexes | Rebuild user indexes with `tenant_id` leading, then add the tenant-leading indexes used by scoped queries. |
| Last-admin trigger | Replace `protect_last_admin()` with the per-tenant version. |
| Feature flags | `feature_flags`, `feature_flag_targets`, the `UNIQUE NULLS NOT DISTINCT (tenant_id, flag_key, user_id)` target index, and the `(tenant_id, user_id) → memberships` FK for tenant-safe user targets (see the identity note above). Without them the evaluator still uses env/compiled defaults, but the admin API fails. |
| Notifications | `notifications` and `notification_preferences` plus indexes; `cmd/migrate` verifies both tables, tenant columns, and the unique preference index. |
| `ON CONFLICT` targets | Every unique index an upsert uses: `idx_jobs_tenant_idempotency_key`, `idx_scheduler_runs_schedule_slot`, the feature-flag target index, the `api_key_scopes`/`webhook_events` primary keys, and the webhook secret/delivery/attempt indexes. |
| Triggers | `trigger_protect_last_admin`, `trigger_audit_events_completion`, `trigger_audit_events_no_delete`, `trigger_audit_events_no_truncate`. `cmd/migrate` reports missing or disabled ones. `audit_events` rejects `DELETE`/`TRUNCATE`; a reviewed retention or erasure job must disable those triggers explicitly as the table owner. |

After the upgrade, access tokens without a `tenant_id` claim are rejected with
`401` (users log in again); old refresh/reset/verification rows are backfilled
to tenant 1 and usable only there. New uploads use
`<tenantID>/<userID>/<uuid>.<ext>` while existing rows keep
`<userID>/<uuid>.<ext>`; if you copy blobs, update `files.storage_path`
atomically.

A fresh schema created out-of-band that already matches the baseline can be
baselined with `force 20260101000000` after verifying the objects, instead of running the
file over populated tables. Never edit a shipped migration after deployment.

## API Endpoints

In production (`TENANT_REQUIRE_HEADER=true`) every API request must send
`X-Tenant-Slug`, except the browser-auth paths that use `?tenant=` and the
OAuth callback (signed `state`). See [Multi-tenancy](#multi-tenancy).
Protected routes accept a JWT or `Authorization: ApiKey` unless noted.

### Auth (public)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/auth/register` | Register new user |
| POST | `/api/v1/auth/login` | Login, returns JWT + refresh token |
| POST | `/api/v1/auth/refresh` | Refresh access token |
| POST | `/api/v1/auth/logout` | Revoke the session of one refresh token |
| POST | `/api/v1/auth/logout-all` | Revoke every session of the current user in every tenant (JWT only) |
| GET | `/api/v1/auth/sessions` | List my live sessions in this tenant (JWT only) |
| DELETE | `/api/v1/auth/sessions/:id` | Revoke one of my sessions (JWT only) |
| POST | `/api/v1/auth/invitations/accept` | Join the invitation's tenant as the signed-in user (JWT only) |
| POST | `/api/v1/auth/invitations/signup` | Create an account from an invitation token (public) |
| POST | `/api/v1/auth/forgot-password` | Request password reset email |
| POST | `/api/v1/auth/reset-password` | Reset password with token (`?tenant=` link fallback) |
| POST | `/api/v1/auth/verify-email` | Verify email with token (`?tenant=` link fallback) |
| POST | `/api/v1/auth/resend-verification` | Resend verification email |
| GET | `/api/v1/auth/google` | Google OAuth redirect (`?tenant=` browser fallback) |
| GET | `/api/v1/auth/google/callback` | Google OAuth callback (tenant from signed state) |

### Users (protected)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/users/me` | Get current user |
| PUT | `/api/v1/users/me` | Update own profile |
| PUT | `/api/v1/users/me/password` | Change password |
| GET | `/api/v1/users/:id` | Get a member of this tenant by user ID (self or admin) |
| GET | `/api/v1/users/` | List members of this tenant (admin only) |
| PUT | `/api/v1/users/:id` | Update own global profile (another id: `403`) |
| DELETE | `/api/v1/users/:id` | Own id: delete the global account (JWT only); another id: remove that membership from this tenant (admin) |

### Invitations (admin)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/invitations/` | Invite an email into this tenant (link emailed) |
| GET | `/api/v1/invitations/` | List pending invitations |
| DELETE | `/api/v1/invitations/:id` | Revoke a pending invitation |

### Files (protected)
Scoped to the request tenant; another tenant's file id returns `404`, including
for admins. Storage is never publicly served.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/files/upload` | Upload file |
| GET | `/api/v1/files/` | List own files (paginated; `includeDeleted=true` shows trash) |
| GET | `/api/v1/files/:id` | Get file info |
| GET | `/api/v1/files/:id/download` | Download file (authenticated) |
| DELETE | `/api/v1/files/:id` | Soft-delete file |
| POST | `/api/v1/files/:id/restore` | Restore own soft-deleted file |

### Notifications (protected)
Scoped to the authenticated user and tenant. Reads need `notifications:read`,
user mutations `notifications:write`, and sending to another user the
admin-only `notifications:send`. Preferences default to enabled for both
channels; only explicit rows are returned. Email is a durable job. See
[docs/api-design.md](docs/api-design.md#notifications).

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/notifications/` | List own notifications (cursor; `unreadOnly=true`) |
| GET | `/api/v1/notifications/unread-count` | Unread badge count |
| GET | `/api/v1/notifications/:id` | Get one own notification |
| POST | `/api/v1/notifications/:id/read` | Mark one read (idempotent) |
| POST | `/api/v1/notifications/read-all` | Mark all read; returns the newly read count |
| POST | `/api/v1/notifications/send` | Admin: notify a user in the tenant |
| GET | `/api/v1/notification-preferences/` | List explicit channel preferences |
| PUT | `/api/v1/notification-preferences/` | Upsert `{type, channel, enabled}` (`channel` is `in_app` or `email`) |

### Admin (protected — admin role)
All operations act on the caller's tenant.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/admin/stats` | Tenant statistics |
| GET | `/api/v1/admin/users` | List users (including deleted) |
| PUT | `/api/v1/admin/users/:id/role` | Update a user role |
| POST | `/api/v1/admin/users/:id/ban` | Ban a user (soft delete) |
| POST | `/api/v1/admin/users/:id/unban` | Unban a user (restore) |
| GET | `/api/v1/admin/files` | List files (including soft-deleted) |
| GET | `/api/v1/admin/files/:id/download` | Download any active file |
| DELETE | `/api/v1/admin/files/:id` | Soft-delete any file (takedown) |
| POST | `/api/v1/admin/files/:id/restore` | Restore any soft-deleted file |
| GET | `/api/v1/admin/features/flags` | List feature flags and effective values |
| GET | `/api/v1/admin/features/flags/:key` | Get a flag and current-tenant targets |
| PUT | `/api/v1/admin/features/flags/:key` | Upsert flag default, kill switch, description |
| DELETE | `/api/v1/admin/features/flags/:key` | Delete flag state (revert to env/compiled) |
| GET | `/api/v1/admin/features/flags/:key/targets` | List current-tenant targets |
| PUT | `/api/v1/admin/features/flags/:key/targets` | Upsert tenant-wide or user target |
| DELETE | `/api/v1/admin/features/flags/:key/targets/:id` | Delete a current-tenant target |

### Webhooks (protected — `webhooks:read` / `webhooks:write`; admin role)
Tenant-scoped. Secrets are returned once on create/rotate and never in reads.
See [docs/webhooks.md](docs/webhooks.md).

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

### API keys (protected — `admin:api_keys:*`; admin role)
Plaintext keys are returned only on create/rotate. A key inherits its
tenant/user binding and an explicit permission-scope subset.

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
| GET | `/readyz` (alias `/health`) | Readiness: `503` when the database is down; a degraded dependency (cache down, pool saturated) stays `200` |
| GET | `/metrics` | Prometheus metrics (`METRICS_AUTH_TOKEN` bearer required in production) |
| GET | `/swagger` | Swagger UI (non-production only) |

### Idempotency

Mutating endpoints accept an optional `Idempotency-Key` header (1–128
characters, `[A-Za-z0-9._:-]`). The decision flowchart lives in
[docs/operations.md](docs/operations.md).

| Case | Result |
|------|--------|
| No header, or GET/HEAD/OPTIONS | Request runs normally |
| First request | Runs and holds an owner-token lock for 2 minutes (above the 60-second HTTP write timeout); only the owner releases it |
| Concurrent duplicate while the lock is held | `409 Conflict` |
| Retry after a 2xx | Replays the cached status, body, and `Content-Type`/`Location`/`ETag` for 24 hours with `Idempotent-Replayed: true`, without running the handler |
| Same key, different body | `422 Unprocessable Entity`, code `idempotency.key_mismatch` (ADR 0008) — non-multipart bodies up to 1 MiB are bound to a SHA-256 of the body; multipart and larger bodies are not |
| Non-2xx response | Not cached |
| Cache error | Fails open: the request runs normally |

Keys are scoped by method, path, tenant ID/slug, and caller (user ID, or IP on
public registration), so they never collide across endpoints, users, or
tenants.

Coverage: registration, profile/password updates, user updates/deletes, file
upload/delete/restore, and admin role/ban/unban/file actions. Token-minting and
credential-verification endpoints (login, refresh, logout, logout-all,
forgot/reset/verify/resend, Google OAuth) and secret-returning endpoints are
excluded; `PUT /users/me/password` is covered because it is a normal
authenticated mutation.

Registration is IP-scoped because no user exists yet: two users behind one NAT
should use distinct keys (a different body returns `422`, never another user's
response). Cached 2xx bodies may contain personal data for 24 hours; size
Redis accordingly. Evicted entries simply let a later retry execute again. Use
`CACHE_DRIVER=redis` so locks and replays work across replicas.

Browser clients must be allowed to send `Idempotency-Key`: the default
`CORS_ALLOW_HEADERS` includes it with `Accept-Language` and `Accept-Version`,
and `CORS_EXPOSE_HEADERS` exposes `Location`, `ETag`, `Idempotent-Replayed`,
`Content-Language`, `API-Version`, `Deprecation`, `Sunset`, and `Link`. Keep
those values if you override the settings.

## Makefile Commands

```bash
make build                        # Build bin/server and bin/migrate (.exe on Windows)
make run                          # Apply schema, then run locally
make migrate-run                  # Apply pending migrations via cmd/migrate
make test                         # Run unit tests
make test-race                    # Unit tests with the race detector (needs cgo; run before merging)
make test-integration             # Run integration tests (Docker/testcontainers, or set TEST_DATABASE_URL)
make check                        # go vet + golangci-lint + arch tests + unit tests
make arch                         # Architecture tests (test/arch)
make lint                         # Run golangci-lint
make docker-run                   # Start with Docker Compose (includes migrate service)
make docker-down                  # Stop Docker Compose
make migrate-down CONFIRM=yes     # golang-migrate CLI: roll back everything (DROPS THE WHOLE SCHEMA)
make migrate-create module=files name=add_x  # New UTC-timestamp migration (<ts>_<owner>_<purpose>)
make sqlc-generate                # Regenerate sqlc code (every internal/modules/*/sqlc.yaml)
make gen module=billing           # Generate a module (also resource= / command= / query= with module=)
make swagger                      # Regenerate Swagger docs
make seed                         # Apply schema, then seed the admin user
make watch                        # Live reload with Air
```

`make migrate-run`, `make run`, and `make seed` use `cmd/migrate`, the only
way to apply migrations (`make migrate-up` was removed). `make migrate-down`
uses the golang-migrate CLI, runs the baseline down migration, and refuses to
run without `CONFIRM=yes`; use it only on a disposable database. `swag` and
`air` run through `go run` at versions pinned in the Makefile
(`SWAG_VERSION`, `AIR_VERSION`); `SWAG_VERSION`/`SQLC_VERSION` match CI.

## Background Jobs

`internal/platform/jobs` is a PostgreSQL-backed durable queue. Delivery is
**at-least-once**: a handler can run again after a retry, panic, or lease
expiry, so handlers must be idempotent. The full job state machine is in
[docs/operations.md](docs/operations.md). `APP_ROLE` decides which processes run
the worker: `all` (default) runs it next to the API, `api` only enqueues, and
`worker` runs the worker and scheduler without `/api` routes.

```mermaid
sequenceDiagram
    participant S as Service
    participant DB as PostgreSQL
    participant W as Worker
    participant H as Handler
    S->>DB: business rows + job row (one transaction, EnqueueTx)
    loop poll
        W->>DB: claim (SKIP LOCKED), attempts + 1, take lease
    end
    W->>H: run with tenant + trace context, 10m timeout
    par heartbeat every 30s
        W->>DB: renew 5m lease (fenced by owner + attempts)
    end
    alt success
        W->>DB: succeeded, payload cleared
    else failure, attempts left
        W->>DB: requeue with backoff (1s doubling, max 1h)
    else attempts exhausted
        W->>DB: dead, payload cleared
    end
```

| Topic | Behavior |
|-------|----------|
| Transactional enqueue | `Queue.EnqueueTx` commits a job with the business transaction. Password-reset and email-verification tokens share a transaction with their `email.send` job, so a crash cannot leave a usable link without a queued email. With transactional email provisioning wired (as in `identity.Build`), registration is atomic too: user, verification token, and `email.send` job commit together, and an outbox failure rolls the account back (fail-closed); without that wiring the handler falls back to a post-commit email. Resend-verification and email-change still use a separate post-commit transaction. |
| Idempotent enqueue | `EnqueueOnce`/`EnqueueOnceTx` take a deterministic `Job.IdempotencyKey` and insert at most one job per `(tenant_id, key)`, returning the existing id and `created=false` for a duplicate. The key is a nullable column with a partial unique index; plain `Enqueue`/`EnqueueTx` do not deduplicate. |
| Tenancy | Enqueue writes `jobs.tenant_id` from `tenant.EffectiveID` (default tenant 1) unless `Job.TenantID` is positive; the worker installs `tenant.WithID` before calling the handler. Claim, lease recovery, retention, and metric labels stay global, so the only tenant-leading jobs index is the `(tenant_id, idempotency_key)` dedupe key; ad-hoc per-tenant inspection scans `idx_jobs_status_run_at`. |
| Tracing | Enqueue stores the active W3C `traceparent`; each attempt starts a new-root `job.process` span linked to the enqueue span. See [docs/tracing.md](docs/tracing.md). |
| Claim and retry | Claims use `FOR UPDATE SKIP LOCKED` and increment `attempts`. Failures retry with exponential backoff (1s doubling up to 1h) until `max_attempts` (default 10), then become `dead`. A handler error wrapped with `jobs.Permanent(err)` (undecodable or invalid payloads) dead-letters immediately with `last_error` prefixed `permanent failure (not retried): `. Unregistered kinds are not special-cased: they retry and dead-letter only when the budget is exhausted, so a rollout can register a handler before pending work dies. |
| Claim errors | Retried in-process with exponential backoff from the poll interval, capped at 30s; `Run` returns only when its context is canceled. |
| Handler timeout | 10m (`WithJobTimeout`; non-positive disables). A timed-out handler is canceled and recorded as a normal failure that consumes an attempt; one that ignores cancellation and returns nil is recorded as succeeded with a warning. The final bookkeeping write uses a detached 5s context. |
| Leases | Each running row stores `lease_expires_at` = now() + the owning worker's lease (`WithLeaseDuration`, default 5m), written on claim and on every heartbeat (`WithHeartbeatInterval`, default 30s); a `CHECK` requires it while `running`. Only a row whose `lease_expires_at < now()` is reclaimed, so workers with different lease durations are safe together. An expired lease is reclaimed by the next claim from any polling worker and consumes an attempt. Heartbeat and completion writes are fenced by owner and attempt count, so a reclaimed worker cannot renew or finish the job, though it may still be running the old handler (duplicate concurrent execution is possible). |
| Inspection | `jobs` stores run time, lease ownership, `attempts`, and `last_error`. Succeeded and dead jobs have `payload` cleared, so reset/verification links are not retained after delivery. |
| Encrypted payloads | Email and password-reset job payloads are sealed by a `jobs.PayloadCodec`: AES-256-GCM with keys derived by HKDF-SHA256 (`jobs/payload/v1` context) from `DATA_ENCRYPTION_KEY` (required in production and different from `JWT_SECRET`; outside production an empty key falls back to `JWT_SECRET`) plus decrypt-only `DATA_ENCRYPTION_PREVIOUS_KEYS`. `jobs.payload` holds a versioned envelope: v2 (`v`, `kid`, `nonce`, `data`) names the sealing key; v1 (no `kid`) still opens by trying every key. Dumps, backups, and replication streams do not expose links. Encrypted email uses kind `email.send.encrypted`; workers register both kinds and legacy plaintext jobs still deliver. A decryption failure (for example a replica missing a key) retries; a malformed envelope is permanent. Workers older than v2 cannot open v2 envelopes, so finish rolling out every worker promptly. Rotation: [docs/operations.md](docs/operations.md#key-rotation). |
| Retention | `succeeded` and `dead` rows whose `updated_at` is older than 30 days are purged hourly, first pass at worker start (`WithRetention`, default `30*24h`, non-positive disables; `WithPurgeInterval`, default `1h`, non-positive ignored). Queued and running jobs are never deleted. Each pass uses a detached 5s context and batches of at most 1,000 rows; failures retry next interval. |
| Webhook delivery | Kind `webhook.delivery`. The delivery row and first job commit atomically; each attempt schedules the next as a **new** job with webhook backoff or `Retry-After`. Permanent 3xx/4xx (except `408`, `425`, `429`) mark the delivery `dead`. Domain events reach it through the `events.<type>.webhooks` durable subscription. Requests follow Standard Webhooks; receivers deduplicate on `webhook-id`. Every attempt is kept in `webhook_delivery_attempts`. An hourly loop deletes terminal deliveries and old inbound events after `WEBHOOK_RETENTION_DAYS`. |
| Shutdown | On SIGINT/SIGTERM the scheduler stops, then the worker stops claiming and cancels running handlers. HTTP, workers, and async work drain in parallel within a **15s** window inside a single **20s** shutdown budget; the remaining 5s covers the tracing flush and closing cache/DB. Handlers must honor cancellation: a non-cooperative handler keeps heartbeating until it returns and can overrun the drain window. See [docs/operations.md](docs/operations.md). |

**Job metrics:** `jobs_claimed_total{kind}`, `jobs_succeeded_total{kind}`,
`jobs_retried_total{kind}`, `jobs_dead_total{kind}`,
`jobs_claim_errors_total`, `jobs_purged_total`, `jobs_queue_lag_seconds{kind}`. **Audit metrics:**
`audit_write_failures_total`, `audit_intent_failures_total`,
`audit_completion_failures_total`, `audit_tx_missing_total`,
`audit_domain_missing_total`. Outcome counters advance only after the matching
database write succeeds. `jobs_dead_total` counts handler failures that
exhaust attempts or are permanent; a lease-expiry dead letter shows as `status = 'dead'` but is
not counted.

## Scheduler and Events

### Scheduler

`internal/platform/scheduler` is optional and global (not tenant-scoped);
enable it with `SCHEDULER_ENABLED=true`. Each schedule targets a tenant in its
`Spec`. The leader/ledger sequence is in [docs/operations.md](docs/operations.md).

| Topic | Behavior |
|-------|----------|
| Leadership | Every process competes for one `scheduler_leases` row. The holder renews every `SCHEDULER_HEARTBEAT_SECS`; a failed renewal steps down before the next pass so two leaders cannot both enqueue. A crashed holder is replaced after `SCHEDULER_LEASE_TTL_SECS`. |
| Ledger | `scheduler_runs` is a unique `(schedule, scheduled_for)` ledger; the ledger insert and job enqueue share a transaction, so no slot is enqueued twice. Rows are purged by the leader after `SCHEDULER_RETENTION_DAYS`. |
| Specs | `Spec.Interval` aligns to Unix epoch boundaries (whole seconds). `Spec.Cron` accepts 5-field expressions (lists, ranges, steps, `0`/`7` Sunday) plus `@hourly`, `@daily`/`@midnight`, `@weekly`, `@monthly`, `@yearly`, `@annually`, in UTC. Set exactly one. |
| Catch-up | `SCHEDULER_CATCHUP`: `none` skips missed slots, `latest` enqueues only the newest, `all` replays up to `SCHEDULER_CATCHUP_LIMIT` per pass. A schedule without ledger history starts at the current slot. `none` still runs a late poll during continuous leadership, so short schedules are not starved. |
| Job | Payload `{"schedule":..., "scheduledFor":..., "data":...}`, idempotency key `scheduler:<name>:<slot>`. `cmd/api` logs the schedule count and stops the scheduler before draining the worker. |
| Modules | Implement `app.ScheduleProvider` (`RegisterSchedules`). |

**Metrics:** `scheduler_runs_total{schedule,outcome}`, `scheduler_leader`,
`scheduler_enqueue_errors_total{schedule}`, `scheduler_runs_purged_total`.

### Events

`internal/platform/events` is a typed in-process bus with optional durable
delivery through the jobs queue.

```mermaid
flowchart LR
    P["PublishTx / Publish"] --> L["Local subscribers<br/>(synchronous)"]
    P --> J["One job per durable handler<br/>key event:&lt;name&gt;:&lt;id&gt;:&lt;handler&gt;:&lt;tenantId&gt;"]
    J --> W["Worker restores tenant,<br/>request, actor"]
    W --> D["Durable handler<br/>(at-least-once)"]
```

| Topic | Behavior |
|-------|----------|
| Building | `events.NewEvent(name, data)` gives a fresh UUID and UTC timestamp. Metadata: `id`, `name`, `tenantId`, `requestId`, `actorId`, `occurredAt`, JSON `data`; missing tenant/request/actor come from the context, explicit values win. |
| Publishing | `PublishLocal` runs subscribers synchronously; `Publish` also enqueues durable subscriptions; `PublishTx` uses the caller's transaction so the business row and fan-out commit together. |
| Subscribing | `Subscribe` registers a local handler. `SubscribeDurable` registers a named handler and job kind (default `events.<event>.<handler>`); each event enqueues one job per durable handler with idempotency key `event:<type>:<id>:<handler>:<tenantId>`. Durable handlers reinstall tenant/request/actor; an undecodable or invalid payload dead-letters (`jobs.Permanent`). |
| Failures | Panics are recovered per handler and errors combined with `errors.Join`; one failing subscriber never skips the others. |
| Wiring | The app calls `RegisterJobs` after modules register; modules implement `app.EventProvider`/`app.JobProvider` only when needed. All provider hooks are nil-safe. |

**Metrics:** `events_published_total{event,mode}`,
`events_handled_total{event,outcome}`, `events_enqueue_errors_total{event}`.

## Adding a New Module

Generate it with `cmd/gen`, then follow
[docs/module-template.md](docs/module-template.md) for route wiring,
permissions, audit, idempotency, tables, contracts, and tests:

```bash
make gen module=billing                            # skeleton + bootstrap entry + init migration
make gen resource=invoice module=billing           # tenant-scoped CRUD slice
make gen command=IssueInvoice module=billing       # CQRS command (+ POST route)
make gen query=ListOverdueInvoices module=billing  # CQRS query (+ GET route)
```

```mermaid
flowchart LR
    A["make gen module=..."] --> B["make gen resource= /<br/>command= / query="]
    B --> C["auto: sqlc, build, vet,<br/>module tests, test/arch<br/>(rollback on failure)"]
    C --> D["Review migration, grants,<br/>vi strings"]
    D --> E["make migrate-run<br/>make swagger"]
    E --> F["make check"]
```

1. Implement `app.Module` (`Name`, `RegisterRoutes`) in
   `internal/modules/<name>/module.go`; add `app.V2Module`
   (`RegisterV2Routes`) only for a breaking v2 surface.
2. Keep handler/service/repository/DTO inside the module; reach other modules
   only through `internal/contracts` or a narrow exported interface.
3. Declare permissions, error codes, flags, and en/vi strings in the module's
   `catalog/` (self-registered through provider hooks) and protect every route
   with `authzhttp.Require(...)`. Do not copy the
   [legacy exceptions](docs/module-template.md#legacy-exceptions-do-not-copy).
4. Write queries in `internal/modules/<name>/queries/*.sql`; the module owns
   its `internal/modules/<name>/sqlc.yaml` (there is no root config). Run
   `make sqlc-generate` (every module) or `go generate ./internal/modules/<name>/`.
5. Generated migrations are `<UTC ts>_<name>_<purpose>`; add other schema
   changes with `make migrate-create module=<name> name=<purpose>`.
6. `make gen module=<name>` registers the module once in
   `internal/bootstrap/modules.go`; module-owned settings go in the module's
   `Settings` struct, filled there from `config`.
7. Add unit and integration tests; `test/arch` rejects cross-module imports
   and new layering violations.
8. Run `make swagger` and `make check` before opening a PR.

Each module with SQL owns its DTOs, repositories, services, handlers, and
generated `sqlc/` package; `admin` is contract-only. There is no shared
generated package and no cross-module sqlc import.

## Environment Variables

See [.env.example](.env.example) for every option and default. Key settings:

| Variable | Purpose |
|----------|---------|
| `APP_ENV` | `local` \| `test` \| `development` \| `staging` \| `production` (log format and production safety checks) |
| `APP_ROLE` | `all` (default: API + job worker + scheduler) \| `api` (HTTP only; jobs are enqueued but not processed) \| `worker` (job worker, scheduler, background loops; only `/healthz`, `/readyz`, `/metrics`) |
| `JOBS_WORKER_KINDS` / `JOBS_WORKER_EXCLUDE_KINDS` | Comma-separated job kinds this process claims / skips (bulkhead workers); every kind must be claimed by some worker, and a kind in both lists fails startup |
| `APP_BODY_LIMIT` | Must exceed `STORAGE_MAX_FILE_SIZE` (multipart overhead) |
| `DB_CONNECT_TIMEOUT` | Database connect timeout in seconds (default 10) |
| `DB_SSLMODE` | `disable`, `allow`, `prefer` rejected in production; use `require` or `verify-full` |
| `DB_QUERY_EXEC_MODE` | pgx execution mode: `cache_statement` (default; behind a transaction-mode PgBouncer needs 1.21+ with `max_prepared_statements > 0`), `cache_describe`, or `describe_exec`; `exec`/`simple_protocol` are rejected (json parameters break) |
| `DB_MIGRATE_HOST` / `DB_MIGRATE_PORT` | `cmd/migrate` only: connect here instead of `DB_HOST`/`DB_PORT`, so migrations (which hold a session advisory lock) bypass a transaction-mode pooler. See [docs/operations.md](docs/operations.md#connection-poolers-pgbouncer) |
| `JWT_SECRET` / `JWT_PREVIOUS_SECRETS` | Active signing key (32+ chars outside local/test; tokens carry its derived `kid`) and comma-separated verify-only retired keys |
| `DATA_ENCRYPTION_KEY` / `DATA_ENCRYPTION_PREVIOUS_KEYS` | AES-256-GCM key for email/password-reset job payloads (32+ chars; required in production and must differ from `JWT_SECRET`; outside production empty falls back to `JWT_SECRET`) and decrypt-only previous keys |
| `REQUIRE_EMAIL_VERIFICATION` | Require a verified email before login/refresh |
| `TENANT_DEFAULT_SLUG` | Tenant for header-less requests in compatibility mode (default `default`) |
| `TENANT_REQUIRE_HEADER` | Require `X-Tenant-Slug` on `/api/v1`/`/api/v2` (default `false`; **must be `true` in production**, recommended in staging) |
| `I18N_DEFAULT_LOCALE` | Fallback locale, `en` or `vi` (default `en`; unsupported values fail startup) |
| `FEATURE_FLAGS` | Comma-separated `key=true\|false` overrides; a bare key means true (unknown keys fail startup) |
| `FEATURE_FLAGS_FORCE_OFF` | Keys that always evaluate false, even during a database outage |
| `FEATURE_FLAGS_CACHE_TTL_SECONDS` / `FEATURE_FLAGS_MAX_STALENESS_SECONDS` | Snapshot freshness and stale-serving bounds (defaults 30/300) |
| `SCHEDULER_ENABLED` | Run the optional PostgreSQL-leader scheduler (default `false`) |
| `SCHEDULER_LEASE_TTL_SECS` / `SCHEDULER_HEARTBEAT_SECS` / `SCHEDULER_POLL_SECS` | Leader lease, renewal, and evaluation timings (defaults 30/10/15) |
| `SCHEDULER_CATCHUP` / `SCHEDULER_CATCHUP_LIMIT` | `none` \| `latest` \| `all` (default `latest`); limit bounds `all` per pass |
| `SCHEDULER_RETENTION_DAYS` | `scheduler_runs` retention (default 30) |
| `FILES_TRASH_RETENTION_DAYS` / `NOTIFICATIONS_RETENTION_DAYS` | Days a soft-deleted file stays restorable before its blob and row are purged (default 30), and days a read notification is kept after being read (default 180; unread are kept). Purged hourly only when `SCHEDULER_ENABLED=true` |
| `STORAGE_DRIVER` | `local` \| `s3` \| `seaweedfs` |
| `CACHE_DRIVER` | `memory` \| `redis` (Redis shares rate limits, login lockouts, session invalidation, and idempotency across replicas) |
| `EMAIL_DRIVER` | `console` \| `smtp` (`console` rejected in production) |
| `METRICS_AUTH_TOKEN` | Required in production; sent as `Authorization: Bearer <token>` |
| `OTEL_ENABLED` | Enable tracing (default `false`); `OTEL_TRACES_EXPORTER` is `otlp` or `none`; `OTEL_EXPORTER_OTLP_ENDPOINT` defaults to `localhost:4317` |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | Admin seeded on API startup (also `make seed`); password 12+ chars in production |
| `WEBHOOK_SECRET_ENCRYPTION_KEY` / `WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS` | AES-256-GCM key for stored webhook secrets (required in production; outside production empty falls back to `JWT_SECRET`) and decrypt-only previous keys |
| `WEBHOOK_*_TIMEOUT_SECS`, `WEBHOOK_RETRY_*_SECS`, `WEBHOOK_MAX_ATTEMPTS`, `WEBHOOK_RETENTION_DAYS`, `WEBHOOK_TIMESTAMP_TOLERANCE_SECS` | Delivery and verification bounds |
| `WEBHOOK_SSRF_ALLOWLIST` | Private IPs/CIDRs/hosts permitted as delivery targets; empty keeps SSRF protection on |
| `<NAME>_FILE` | Read a secret from a file (Docker/Kubernetes secret mounts) for `JWT_SECRET`, `JWT_PREVIOUS_SECRETS`, `DB_PASSWORD`, `DATA_ENCRYPTION_KEY`, `DATA_ENCRYPTION_PREVIOUS_KEYS`, `WEBHOOK_SECRET_ENCRYPTION_KEY`, `WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS`, `SMTP_PASSWORD`, `GOOGLE_CLIENT_SECRET`, `ADMIN_PASSWORD`, `METRICS_AUTH_TOKEN`, `STORAGE_S3_SECRET_KEY`, `REDIS_URL`; a trailing newline is trimmed, and setting both `<NAME>` and `<NAME>_FILE` fails startup |

## Pagination

List endpoints return `{object, url, data, hasMore, nextCursor}`. When
`hasMore` is true, pass `nextCursor` as `startingAfter` on the next request.
`limit` is clamped to 100. See [docs/api-design.md](docs/api-design.md).

## Testing

```bash
make test                 # unit tests (no Docker)
make test-integration     # testcontainers (Docker)

# Without Docker, against an existing PostgreSQL. Apply the schema first:
# some integration tests (audit, jobs, scheduler, pgx tracing) use the
# database as-is and need the tables cmd/migrate creates.
DB_HOST=127.0.0.1 DB_PORT=5432 DB_DATABASE=postgres DB_USERNAME=user DB_PASSWORD=pass \
  DB_SSLMODE=disable go run ./cmd/migrate   # or: make migrate-run with matching .env
TEST_DATABASE_URL='postgres://user:pass@127.0.0.1:5432/postgres?sslmode=disable' \
  go test -tags=integration -race -count=1 ./...
```

Module integration tests create and migrate their own disposable schema on
that server; the platform tests above skip when the schema is missing. Use a
disposable database.

Testcontainers pulls the Docker client as a test-only dependency. `govulncheck`
reports no called vulnerabilities, but GitHub may show advisories for that
client until upstream ships a patch; it is not linked into the API binary.

## Scope and Remaining Work

This is a boilerplate, not a finished multi-tenant SaaS platform. The modular
monolith, durable jobs, audit trail, permission-based authorization, single
migration entrypoint, and tenant foundation exist.

| Area | Status |
|------|--------|
| Tenant isolation | Implemented for identity, files, webhooks, apikeys, and notifications (scoped SQL/indexes, tenant-bound OAuth state, tenant-aware sessions, tenant-prefixed storage keys, source-resolved inbound webhooks). Row-level security and per-tenant limits are future work. |
| Notifications | In-app feed, per-type channel preferences, durable email channel committed with the in-app row; at-least-once delivery. |
| Tracing | HTTP, sanitized pgx queries, and durable jobs; disabled by default (`OTEL_ENABLED=true` plus a collector to enable). |
| Delivery | Metrics, Kubernetes manifests, GHCR release workflow, and an optional gated CD workflow exist; full promotion/rollout automation does not. |

## Security Notes

| Area | Guarantee |
|------|-----------|
| Refresh tokens | Hashed, atomic single-use, bound to their session and the user's token version, rotated on every refresh. Reusing a rotated token revokes the whole session (RFC 9700). |
| Access tokens | Carry `sid`, `token_version`, and the membership role; password change/reset, email change, logout-all, session revocation, ban, removal, and role change invalidate them immediately. Validated against the session, membership, user, and tenant through a generation-keyed session cache (use Redis in production so invalidation is shared). |
| Tenant claim | A conflicting explicit `X-Tenant-Slug` returns `403`; a missing/zero/negative claim `401`. Unknown or suspended tenants are rejected with `404` before authentication; a cached suspended snapshot returns `401` until the 5-second TTL expires. |
| Emails | Normalized and unique case-insensitively among live accounts globally. Email changes require the current password, reset verification, revoke sessions, and send a fresh link. An email banned in a tenant cannot re-register or be invited there, even after the banned account deleted itself; otherwise a self-deleted address can re-register. |
| Logout | `POST /auth/logout` revokes one session by possession of its refresh token; `/auth/logout-all` revokes every session in every tenant and bumps the token version. |
| Files | Never served from a public directory; downloads require authentication and ownership (or admin). Metadata and blob keys are tenant-scoped; another tenant's id returns `404`. |
| Outbound webhooks | SSRF-checked at dial time: private, loopback, link-local, multicast, CGNAT, and reserved ranges blocked unless allowlisted in `WEBHOOK_SSRF_ALLOWLIST`; DNS resolved once and pinned; proxies and redirects disabled. Standard Webhooks HMAC-SHA256 signing (`webhook-signature: v1,<base64>` over `id.timestamp.body`) with a per-endpoint `whsec_` secret; receivers deduplicate on `webhook-id`. |
| Webhook secrets | AES-256-GCM at rest with `WEBHOOK_SECRET_ENCRYPTION_KEY` (required in production); plaintext only on create/rotate. Envelopes name their key (`kid`), so the encryption key rotates in two phases through `WEBHOOK_SECRET_ENCRYPTION_PREVIOUS_KEYS` without making stored secrets unreadable. |
| Signing and encryption keys | JWTs carry a `kid`; `JWT_PREVIOUS_SECRETS` keeps retired keys verify-only. Job payloads use a dedicated `DATA_ENCRYPTION_KEY`, so signing and encryption keys rotate independently. Procedure: [docs/operations.md](docs/operations.md#key-rotation). |
| Authorization | `authz.Authorize` is the single service-level path and fails closed: a call with neither a subject nor an explicit `authz.SystemContext` (job handlers, admin seed) is `403`. |
| Idempotency | Cache scope includes tenant ID and slug, so a key/body from one IP cannot replay across tenants. |
| Production startup | Rejects the sample JWT secret and default admin password, a missing `DATA_ENCRYPTION_KEY` or one equal to `JWT_SECRET`, console email, wildcard CORS, insecure frontend/OAuth URLs, missing metrics auth, `DB_SSLMODE=disable`, `TENANT_REQUIRE_HEADER=false`, and a body limit smaller than the upload size. |
