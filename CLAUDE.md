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
on a fresh PostgreSQL service, then runs `cmd/migrate` twice. Reproduce the
fresh-DB check locally against a disposable database before merging.

## Architecture

**Modular monolith.** One deployable; modules own route tables; platform
packages own cross-cutting mechanics.

```
cmd/api/main.go             bootstrap + module registration + graceful shutdown
cmd/migrate/main.go         the only schema initializer/migrator
cmd/seed/main.go            admin seeder (schema must exist)
internal/app/               Fiber app, global middleware, Module registry
internal/modules/           identity, files, admin (route tables)
internal/platform/          authz, audit, jobs, and future shared mechanics
internal/handler|service|repository|dto|sqlc   current internals (strangler phase)
internal/middleware|session
pkg/                        apperror, response, database, cache, storage, email, ...
migrations/000001_init_schema.*   single consolidated up/down schema
```

**Request flow:** `Client → app middleware → module route → handler → service → repository → sqlc → PostgreSQL`.

**Dependency rules**
- A module must not import another module's internals. Use `internal/contracts`
  or a narrow exported interface.
- `platform`/`pkg` must not import business modules.
- One module owns each table; cross-module reads use a contract/read model,
  not another module's repository.
- Enforce with `test/arch/imports_test.go`; do not weaken it.

**Schema ownership**
- Only `cmd/migrate` applies DDL. API/seed assume the schema exists.
- `cmd/migrate` uses `config.LoadDatabase` (DB_* only; no JWT/admin/SMTP
  secrets).
- Fresh installs: one `migrations/000001_init_schema.up.sql`.
- Existing databases: keep their `schema_migrations` history and add a
  migration above the current version (a fully migrated old deployment is at 3,
  so `000004`+) with only the missing objects. Never point an old database at
  the new `000001`, reset `schema_migrations`, or edit a shipped migration.

**Scope:** this is a boilerplate, not a finished multi-tenant SaaS platform.
Tenant isolation, per-module sqlc generation, tracing/CD, and moving the current
handler/service/repository internals into modules remain future work.

## Error Handling

- Return `*apperror.AppError` from services/handlers. Prefer the typed catalog:
  `apperror.NewCode(apperror.CodeUserEmailTaken, "public detail")`.
- `pkg/apperror` renders RFC 9457 `application/problem+json` with `type`,
  `title`, `status`, `code`, `detail`, `instance`, `requestId`, `timestamp`,
  and optional `errors[]`.
- `detail` must be public text; never serialize internal causes, SQL, or stack
  traces. Sentinels: `apperror.ErrNotFound`.
- Permission failures should use `authz.Require`; it returns
  `permission.denied`.

## Response & Pagination

Use `pkg/response`; never write raw JSON in handlers:
- `response.Success`, `response.Created`, `response.NoContent`
- `response.List(c, data, hasMore, nextCursor...)` →
  `{object,url,data,hasMore,nextCursor}`

Cursor pagination: `limit` (max 100) + `startingAfter`; repositories use
`(created_at, id)` indexes; handlers return `nextCursor` when `hasMore`.
Offset helpers are for small admin/export tables only.

## Key Patterns

- **Auth/session:** JWT carries `token_version`; `session.Store` validates it
  with generation-keyed invalidation. Refresh tokens are atomic single-use and
  version-bound. `pkg/token` pins HS256 and requires `exp`.
- **Jobs:** `internal/platform/jobs` is the durable queue; delivery is
  at-least-once, so handlers must be idempotent. Services enqueue email/webhook
  work; password-reset and verification tokens are committed with their
  `email.send` job in one transaction (`QueuedSender.SendTx`), so an enqueue
  failure rolls the token back. A worker retries with backoff and dead-letters
  after `max_attempts`.
  Claim errors are retried in-process with backoff capped at 30s. Handlers have
  a 10m default timeout (`WithJobTimeout`, non-positive disables it), and
  unknown kinds retry until `max_attempts` before dead-lettering. Heartbeats
  renew a 5m lease every 30s; expired leases are reclaimed. Handlers must honor
  context cancellation: the worker cannot preempt a handler that ignores it,
  heartbeats continue until it returns, and a non-cooperative handler can
  exceed the shutdown drain window (the app waits up to 10s). Bookkeeping writes
  are fenced by `attempts`, so a reclaimed worker cannot finish the job, though
  it may still be executing the old handler. Do not use `async.Go` for durable
  work.
- **Audit:** `platform/audit.Middleware` records successful mutating requests;
  use `audit.Recorder` for explicit domain events.
- **Authz:** permissions live in `platform/authz`; roles map to permissions.
  Protect routes with `authz.Require(authz.PermissionX)`.
- **Transactions:** `database.TxManager.WithTx` scopes a transaction to one
  module/aggregate. Do not span modules; use jobs/events.
- **Drivers:** storage (`local|s3|minio`), cache (`memory|redis`), email
  (`console|smtp`) are selected by config factories.
- **Rate limits:** tiered, cache-backed in `internal/app`; authenticated
  requests are keyed per user, unauthenticated per IP.

## Adding a Module

1. Create `internal/modules/<name>/module.go` implementing `app.Module`.
2. Keep handler/service/repository/DTO inside the module.
3. Add permissions and protect mutation routes.
4. Add queries and run `make sqlc-generate`.
5. Add a migration only for a deployed DB; otherwise update the single init
   schema.
6. Register the module in `cmd/api/main.go`.
7. Add unit + integration tests; `test/arch` must stay green.
8. Run `make swagger` and `make check`.

## Testing

- Standard library `testing` + `testify` where it improves assertions.
- Unit tests must not need Docker. Integration tests use `-tags=integration`
  and either Testcontainers or `TEST_DATABASE_URL`.
- Run `go test -race ./...` before merging.
- Architecture tests live in `test/arch`.
```
