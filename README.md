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
- **Database**: PostgreSQL 17 with [pgxpool](https://github.com/jackc/pgx)
- **Query**: [sqlc](https://sqlc.dev/) (type-safe SQL code generation)
- **Migration**: [golang-migrate](https://github.com/golang-migrate/migrate) via one `cmd/migrate` entrypoint (single init schema)
- **Auth**: JWT ([golang-jwt](https://github.com/golang-jwt/jwt)) + Google OAuth 2.0
- **Validation**: [go-playground/validator](https://github.com/go-playground/validator)
- **Logging**: slog (stdlib structured logging)
- **Docs**: Swagger/OpenAPI via [swaggo](https://github.com/swaggo/swag)
- **Linter**: [golangci-lint v2](https://golangci-lint.run/)
- **Cache**: In-memory or Redis
- **Storage**: Local filesystem or S3/MinIO
- **Email**: SMTP or console (dev)
- **Metrics**: Prometheus
- **Container**: Docker + Docker Compose

## Architecture

The project is a **modular monolith**: one deployable, module-owned route tables, and
platform packages for cross-cutting mechanics.

```
cmd/api/main.go                     Bootstrap, module registration, graceful shutdown
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
  admin/                            Back-office facade
                                    (dto + service + handler, contracts-only)
internal/contracts/                 Narrow cross-module identity/files admin interfaces (stdlib only)
internal/platform/
  authz/                            Permission catalog + Fiber middleware
  audit/                            Audit event recorder + mutating-request middleware
  jobs/                             Durable PostgreSQL jobs, worker, leases, retries, dead-letter
  session/                          Token-version/session validation
  tenant/                           Tenant resolution from X-Tenant-Slug
internal/platform/middleware/      JWT, rate limit, logger, recovery, security headers, metrics, idempotency
pkg/
  apperror/                         RFC 9457 errors + typed error catalog
  response/                         JSON response helpers
  database/                         Pool, TxManager, migration runner
  validator/ token/ cache/ storage/ email/ pagination/ logger/ health/ oauth/ metrics/ async/
migrations/000001_init_schema.*     Single consolidated init schema (up/down)
docs/                               Swagger + architecture/API/module/ops docs
deploy/k8s/                         Kubernetes production manifests (see deploy/k8s/README.md)
```

See [docs/architecture.md](docs/architecture.md) for dependency rules and the
strangler status, [docs/api-design.md](docs/api-design.md) for the API design
system (errors, pagination, idempotency, status codes),
[docs/module-template.md](docs/module-template.md) for the canonical module
layout and scaffold, and [docs/operations.md](docs/operations.md) for the
operations runbook.

Request flow: `Client → app middleware → module route → handler/service/repository → sqlc → PostgreSQL`.

**Schema ownership:** only `cmd/migrate` applies DDL. The API and seed commands
assume the schema exists, so replicas never race on migrations. Fresh installs
run the single `000001_init_schema.up.sql`; add new numbered migrations only
after a database is deployed.

## Multi-tenancy

The schema, request pipeline, and identity module are tenant-scoped. One user
belongs to exactly one tenant, and no identity read or write crosses tenants.

- The baseline seeds a `default` tenant with id `1`. Requests without
  `X-Tenant-Slug` resolve to `TENANT_DEFAULT_SLUG` (`default`), preserving the
  previous single-tenant behavior. Set `TENANT_REQUIRE_HEADER=true` to reject
  requests that omit the header.
- `X-Tenant-Slug` selects the request tenant on `/api/v1` routes. Invalid,
  unknown, or suspended slugs return `404` `tenant not found`.
- Public auth calls operate on the resolved tenant: registration, login,
  forgot/reset password, resend/verify email, and refresh. A client that
  registered in a non-default tenant must send the same header on those calls.
- Access tokens carry a `tenant_id` claim. Authenticated requests use the
  claim; if an explicit `X-Tenant-Slug` resolves to a different tenant, the
  request fails with `403 permission.denied` (`tenant mismatch`).
- All identity SQL is scoped by `tenant_id`: users by id, email, Google ID,
  admin lists/stats, and refresh/reset/verification tokens. A cross-tenant id
  or token behaves as not found (admin/user APIs return `404`, token flows
  return `400`/`401`).
- Emails and Google IDs are unique per tenant. The last-admin database trigger
  and the application check protect each tenant independently.
- Identity and files are tenant-isolated: all user/admin/stats and
  refresh/reset/verification queries filter `tenant_id`; every file metadata
  read/write, admin list/stats, download, delete, and restore filters the
  request tenant. Cross-tenant ids behave as `404`. Uploaded blobs use
  `<tenantID>/<userID>/<uuid>.<ext>`.
- Google OAuth binds the tenant into the OAuth `state` (and state cookie); the
  callback uses that tenant, not `X-Tenant-Slug`, and refuses a tenant suspended
  during consent.
- Tokens issued before multi-tenancy have no `tenant_id` claim and are rejected
  fail-closed. Rotate access tokens after upgrading (users must log in again);
  refresh/reset/verification rows default to tenant 1.
- The session validator compares the JWT `tenant_id` with the stored user tenant
  and rejects suspended tenants within the 5-second session-cache TTL.
- **Remaining:** row-level security and per-tenant limits/quotas.

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

Services: API (:8080), PostgreSQL (:5432), Redis (:6379). Optional S3-compatible MinIO: `docker compose --profile s3 up` (:9000/:9001).

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

Production-oriented manifests live in [`deploy/k8s`](deploy/k8s/README.md), and
[`.github/workflows/release.yml`](.github/workflows/release.yml) builds and
pushes multi-arch images to GHCR on `v*` tags. PostgreSQL and Redis are expected
to be managed services; no stateful database is bundled in the app Deployment.

The migration Job is deliberately outside the app kustomization. Apply and
await it before `kubectl apply -k deploy/k8s`; see the deploy README for the
full ordered flow, placeholders, secret creation, verification, and rollback.

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
secrets. `DB_SSLMODE=disable` is rejected when `APP_ENV=production`. After
migrating, the command verifies that every required table exists and fails with
an actionable error if it finds a legacy partial schema.

### Upgrading an existing database

`000001_init_schema` is a **fresh-install baseline**. It replaces the old
shipped migrations `000001`–`000003` and also creates the `jobs` and
`audit_events` tables.

A database that already applied the old migrations has a different
`schema_migrations` history (normally version 3 if all old migrations were
applied). Do not point it at the new `000001`, and never reset
`schema_migrations` to force the baseline through. Keep the applied history and
add a numbered migration containing only the missing tables/indexes. The new
version must be higher than the version already recorded, so a fully migrated
old deployment needs `000004` or later.

Because this refactor removed the old `000002`/`000003` source files, restore
them from git history first (do not edit them): golang-migrate requires the
recorded version to exist in the source before it will advance. Add `000004`
(or later) for `jobs`, `audit_events`, and any other missing objects, then run
`cmd/migrate`. `cmd/migrate` prints this guidance when it detects a legacy
version whose source files are missing. Alternatively, apply the missing DDL
manually with `psql` and baseline the version; do not run the new `000001` over
the old schema.

The multi-tenant foundation also changes `000001` (new `tenants` table,
`tenant_id` columns, per-tenant indexes, and the per-tenant last-admin
trigger). An existing database does **not** get these objects from the baseline.
Create the next numbered migration above its recorded version (or apply
reviewed DDL manually) that:

1. creates `tenants` and inserts `('default','Default')` as the first row so it
   receives id 1;
2. adds `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)` to
   `users`, `files`, `refresh_tokens`, `password_reset_tokens`,
   `email_verification_tokens`, `jobs`, and `audit_events`;
3. rebuilds the user indexes with `tenant_id` leading, then adds the
   tenant-leading indexes used by scoped queries;
4. replaces `protect_last_admin()` with the per-tenant version.

Keep the default tenant id 1 unless you also change the column defaults and
backfill every row. Fresh installs get all of this from `000001`.

Identity/files isolation needs no further schema change: the tenant columns and
indexes already exist. Existing access tokens without a `tenant_id` claim are
rejected with 401, so clients must log in again. Existing refresh/reset/
verification rows were backfilled to tenant 1 and remain usable only there.
New uploads use the blob key `<tenantID>/<userID>/<uuid>.<ext>`, while existing
rows keep their old `<userID>/<uuid>.<ext>` path; if you copy blobs, update
`files.storage_path` atomically.

If a fresh schema was created out-of-band and already matches `000001`,
baseline it manually after verifying the objects (for example, set version 1
with the golang-migrate CLI's `force 1`) instead of running the file over
populated tables. Never edit a shipped migration after deployment.

## API Endpoints

Public and authenticated API requests may send `X-Tenant-Slug`; omitting it
uses the configured default tenant. Login, registration, refresh, password
reset, and email verification all resolve the tenant from this header (or the
default), so clients of a non-default tenant must send it consistently.
Authenticated requests must not send a tenant that differs from the JWT
`tenant_id` claim. Google OAuth uses the tenant bound to its `state` instead of
the header on the callback.

### Auth (public)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/auth/register` | Register new user |
| POST | `/api/v1/auth/login` | Login, returns JWT + refresh token |
| POST | `/api/v1/auth/refresh` | Refresh access token |
| POST | `/api/v1/auth/logout` | Revoke one refresh token |
| POST | `/api/v1/auth/logout-all` | Revoke every refresh token for the current user |
| POST | `/api/v1/auth/forgot-password` | Request password reset email |
| POST | `/api/v1/auth/reset-password` | Reset password with token |
| POST | `/api/v1/auth/verify-email` | Verify email with token |
| POST | `/api/v1/auth/resend-verification` | Resend verification email |
| GET | `/api/v1/auth/google` | Google OAuth redirect |
| GET | `/api/v1/auth/google/callback` | Google OAuth callback |

### Users (protected — JWT required)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/users/me` | Get current user |
| PUT | `/api/v1/users/me` | Update own profile |
| PUT | `/api/v1/users/me/password` | Change password |
| GET | `/api/v1/users/:id` | Get user by ID (self or admin) |
| GET | `/api/v1/users/` | List users (admin only) |
| PUT | `/api/v1/users/:id` | Update user (admin or self) |
| DELETE | `/api/v1/users/:id` | Delete user (admin or self) |

### Files (protected — JWT required)
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

### Admin (protected — admin role required)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/admin/stats` | System statistics |
| GET | `/api/v1/admin/users` | List all users (including deleted) |
| PUT | `/api/v1/admin/users/:id/role` | Update user role |
| POST | `/api/v1/admin/users/:id/ban` | Ban user (soft delete) |
| POST | `/api/v1/admin/users/:id/unban` | Unban user (restore) |
| GET | `/api/v1/admin/files` | List all files (including soft-deleted) |
| GET | `/api/v1/admin/files/:id/download` | Download any active file |
| DELETE | `/api/v1/admin/files/:id` | Soft-delete any file (takedown) |
| POST | `/api/v1/admin/files/:id/restore` | Restore any soft-deleted file |

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
- keys are scoped by method, path, and caller (user ID, or IP for the public
  registration route), so the same key on another endpoint or user does not
  collide

Browser clients must be allowed to send `Idempotency-Key`. The default
`CORS_ALLOW_HEADERS` includes it, and the default `CORS_EXPOSE_HEADERS` exposes
`Location`, `ETag`, and `Idempotent-Replayed`. If you override those settings,
keep the relevant values.

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
make migrate-create name=xxx      # Create a new migration after deployment
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
When transactional email provisioning is wired (as in `cmd/api/main.go`),
registration is atomic as well: the user row, its verification token, and the
`email.send` job commit together, and an outbox write failure rolls the account
back (fail-closed). Without that wiring, the handler falls back to a
post-commit verification email. Resend-verification and email-change flows
still create their token and email in a separate transaction after the account
already exists.

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
  dead-lettered. Cancellation is cooperative: the worker cannot preempt a
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
  `jobs_claim_errors_total`, `jobs_purged_total`, and
  `audit_write_failures_total`. Outcome counters advance only after the
  corresponding database write succeeds. `jobs_dead_total` counts handler
  failures that exhaust their attempts; a job dead-lettered by lease expiry is
  visible as `status = 'dead'` in the table but is not counted by that
  handler-outcome metric.
- On SIGINT/SIGTERM the worker stops claiming, cancels running handlers, and
  the app waits up to 10s for them to drain. Handlers must honor context
  cancellation: the worker cannot preempt a handler that ignores it, heartbeats
  continue until that handler returns, and a non-cooperative handler can exceed
  the shutdown drain window.

## Adding a New Module

Use `scripts/new-module.sh <name>` to create the compile-safe layout, then
follow [docs/module-template.md](docs/module-template.md) for route wiring,
permissions, audit, idempotency, tables, contracts, tests, and the full
checklist.

1. Create `internal/modules/<name>/module.go` implementing `app.Module`
   (`Name`, `RegisterRoutes`).
2. Keep the module's handler/service/repository/DTO inside its directory.
   Do not import another module's internals; use `internal/contracts` or a
   narrow exported interface.
3. Add permissions to `internal/platform/authz` and protect every route with
   `authz.Require(...)`. The legacy exceptions are documented in the
   [module template](docs/module-template.md#legacy-exceptions-do-not-copy);
   do not copy them.
4. Write queries in the module-scoped
   `internal/modules/<name>/queries/<name>.sql` file, add a sqlc block in
   `sqlc.yaml` for the module, and run `make sqlc-generate`.
5. For a deployed database, add a numbered migration. For this boilerplate's
   fresh installs, update the single `000001_init_schema` file.
6. Register the module once in `cmd/api/main.go`.
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
- `TENANT_DEFAULT_SLUG` — tenant for requests without `X-Tenant-Slug` (default `default`)
- `TENANT_REQUIRE_HEADER` — require `X-Tenant-Slug` on `/api/v1` requests (default `false`)
- `STORAGE_DRIVER` — `local` | `s3` | `minio`
- `CACHE_DRIVER` — `memory` | `redis` (Redis shares rate-limit counters, login lockouts, session invalidation, and idempotency locks/replays across replicas)
- `EMAIL_DRIVER` — `console` | `smtp` (`console` is rejected in production)
- `DB_SSLMODE` — `disable` is rejected in production; use `require` or `verify-full`
- `METRICS_AUTH_TOKEN` — required in production; sent as `Authorization: Bearer <token>`
- `ADMIN_EMAIL` / `ADMIN_PASSWORD` — Auto-seed admin user on API startup (also available via `make seed`; password must be 12+ chars in production)

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

- Tenant isolation is implemented for identity and files, including
  tenant-scoped SQL, admin lists/stats, tenant-bound Google OAuth state,
  tenant-aware sessions, and tenant-prefixed storage keys. Row-level security
  and per-tenant limits remain future work.
- Distributed tracing, plus environment promotion/rollout automation: metrics,
  Kubernetes manifests (`deploy/k8s`), and the GHCR release workflow exist, but
  tracing and a full CD platform do not.

All bounded-context internals are module-owned under `internal/modules`;
identity and files own their `queries/` and generated `sqlc/` packages, and
`admin` composes through `internal/contracts`. No shared generated package and
no legacy handler/service/DTO/repository packages remain.

## Security notes

- Refresh tokens are hashed, atomic/single-use, bound to the user's token
  version, and rotated on every refresh.
- Access tokens carry `token_version`; password changes/resets, bans,
  unbans, and role changes invalidate them immediately.
- Access tokens also carry `tenant_id`. A request whose explicit
  `X-Tenant-Slug` resolves to a different tenant is rejected with 403; a token
  without a tenant claim, or one for a suspended tenant, is rejected with 401.
- Access tokens are validated against the current user record through a
  generation-keyed session cache. Use Redis in production so invalidation is
  shared across replicas.
- Emails are normalized and unique case-insensitively among active accounts
  within a tenant; email changes require the current password, reset
  verification, revoke sessions, and send a fresh verification link.
- Uploaded files are never served from a public directory; downloads require
  authentication and ownership (or admin role). File metadata and blob keys are
  tenant-scoped, and another tenant's id returns 404.
- Banned emails cannot be reused through registration or Google login;
  self-deleted accounts can still be re-registered.
- `POST /auth/logout` revokes one refresh token; use `/auth/logout-all` to bump
  the token version and invalidate access tokens immediately.
- Production startup rejects the sample JWT secret/default admin password,
  console email, wildcard CORS, insecure frontend/OAuth URLs, missing metrics
  auth, `DB_SSLMODE=disable`, and a body limit smaller than the upload size.
