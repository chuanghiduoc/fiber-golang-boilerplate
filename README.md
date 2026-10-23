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
  identity/                         Auth, users, sessions, tokens (route table)
  files/                            Upload, download, soft delete, restore
  admin/                            Back-office facade
internal/platform/
  authz/                            Permission catalog + Fiber middleware
  audit/                            Audit event recorder + mutating-request middleware
  jobs/                             Durable PostgreSQL jobs, worker, leases, retries, dead-letter
internal/handler/                   Current handler internals (being moved into modules)
internal/service/                   Current service internals
internal/repository/                Current repository internals
internal/dto/                       Current DTOs
internal/sqlc/                      Generated code (DO NOT EDIT — `make sqlc-generate`)
internal/middleware/                JWT, rate limit, logger, recovery, security headers, metrics
internal/session/                   Token-version/session validation
pkg/
  apperror/                         RFC 9457 errors + typed error catalog
  response/                         JSON response helpers
  database/                         Pool, TxManager, migration runner
  validator/ token/ cache/ storage/ email/ pagination/ logger/ health/ oauth/ metrics/ async/
migrations/000001_init_schema.*     Single consolidated init schema (up/down)
queries/                            sqlc query files
docs/                               Generated Swagger docs
```

Request flow: `Client → app middleware → module route → handler/service/repository → sqlc → PostgreSQL`.

**Schema ownership:** only `cmd/migrate` applies DDL. The API and seed commands
assume the schema exists, so replicas never race on migrations. Fresh installs
run the single `000001_init_schema.up.sql`; add new numbered migrations only
after a database is deployed.

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

If a fresh schema was created out-of-band and already matches `000001`,
baseline it manually after verifying the objects (for example, set version 1
with the golang-migrate CLI's `force 1`) instead of running the file over
populated tables. Never edit a shipped migration after deployment.

## API Endpoints

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
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/files/upload` | Upload file |
| GET | `/api/v1/files/` | List own files (paginated; `includeDeleted=true` shows trash) |
| GET | `/api/v1/files/:id` | Get file info |
| GET | `/api/v1/files/:id/download` | Download file (authenticated; storage is not publicly served) |
| DELETE | `/api/v1/files/:id` | Delete file (soft) |
| POST | `/api/v1/files/:id/restore` | Restore own soft-deleted file |

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
- On SIGINT/SIGTERM the worker stops claiming, cancels running handlers, and
  the app waits up to 10s for them to drain. Handlers must honor context
  cancellation: the worker cannot preempt a handler that ignores it, heartbeats
  continue until that handler returns, and a non-cooperative handler can exceed
  the shutdown drain window.

## Adding a New Module

1. Create `internal/modules/<name>/module.go` implementing `app.Module`
   (`Name`, `RegisterRoutes`).
2. Keep the module's handler/service/repository/DTO inside its directory.
   Do not import another module's internals; use `internal/contracts` or a
   narrow exported interface.
3. Add permissions to `internal/platform/authz` and protect mutation routes
   with `authz.Require(...)`.
4. Write queries in a module-scoped `queries/<name>.sql` file and run
   `make sqlc-generate`.
5. For a deployed database, add a numbered migration. For this boilerplate's
   fresh installs, update the single `000001_init_schema` file.
6. Register the module once in `cmd/api/main.go`.
7. Add module unit tests and an integration test; the architecture test in
   `test/arch` rejects cross-module imports.
8. Run `make swagger` and `make check` before opening a PR.

> The current `identity`, `files`, and `admin` modules wrap the existing
> handler/service/repository packages. Moving those internals into the module
> directories is the next strangler phase; new modules should start directly
> in the module layout.

## Environment Variables

See [.env.example](.env.example) for all available configuration options with defaults.

Key settings:
- `APP_ENV` — `local` | `test` | `development` | `staging` | `production` (affects logging format and production-only safety checks)
- `APP_BODY_LIMIT` — must be greater than `STORAGE_MAX_FILE_SIZE` (multipart overhead)
- `DB_CONNECT_TIMEOUT` — database connect timeout in seconds (default 10)
- `REQUIRE_EMAIL_VERIFICATION` — require a verified email before login/refresh
- `STORAGE_DRIVER` — `local` | `s3` | `minio`
- `CACHE_DRIVER` — `memory` | `redis` (Redis shares rate-limit counters, login lockouts, and session invalidation across replicas)
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
monolith, durable jobs, audit trail, permission-based authorization, and single
migration entrypoint exist. Remaining work:

- Tenant isolation: tenant-scoped data, query enforcement/row-level security,
  and per-tenant limits.
- Per-module sqlc generation: `internal/sqlc` is still one shared package.
- Tracing and CD: metrics exist, but distributed tracing and deployment
  pipelines do not.
- Moving `internal/handler|service|repository|dto` internals into the module
  directories (the strangler phase noted above).

## Security notes

- Refresh tokens are hashed, atomic/single-use, bound to the user's token
  version, and rotated on every refresh.
- Access tokens carry `token_version`; password changes/resets, bans,
  unbans, and role changes invalidate them immediately.
- Access tokens are validated against the current user record through a
  generation-keyed session cache. Use Redis in production so invalidation is
  shared across replicas.
- Emails are normalized and unique case-insensitively among active accounts;
  email changes require the current password, reset verification, revoke
  sessions, and send a fresh verification link.
- Uploaded files are never served from a public directory; downloads require
  authentication and ownership (or admin role).
- Banned emails cannot be reused through registration or Google login;
  self-deleted accounts can still be re-registered.
- `POST /auth/logout` revokes one refresh token; use `/auth/logout-all` to bump
  the token version and invalidate access tokens immediately.
- Production startup rejects the sample JWT secret/default admin password,
  console email, wildcard CORS, insecure frontend/OAuth URLs, missing metrics
  auth, `DB_SSLMODE=disable`, and a body limit smaller than the upload size.
