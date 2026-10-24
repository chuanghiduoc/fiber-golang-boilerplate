# Architecture

This repository is a **modular monolith**: one deployable, module-owned HTTP
surfaces, and platform packages for cross-cutting mechanics. The goal is to
support many business modules without microservice overhead.

## Runtime shape

```
cmd/api        bootstrap, DI, worker lifecycle, graceful shutdown
cmd/migrate    the only DDL entrypoint
cmd/seed       standalone admin seeder (schema must already exist)

internal/app   composition root: Fiber, middleware, infra routes, module registry
internal/modules
  identity     auth, users, sessions, tokens, admin seed
  files        upload, metadata, download, soft delete, restore
  admin        back-office facade
internal/platform
  authz        permission catalog + route middleware
  audit        audit recorder + mutating-request middleware
  jobs         durable PostgreSQL queue, worker, leases, retention
  session      token-version/session cache
internal/middleware   HTTP middleware (JWT, rate limit, logger, idempotency, ...)
internal/handler|service|repository|dto|sqlc   legacy strangler layer
pkg            reusable infrastructure (errors, response, database, cache, ...)
```

Request flow:

```
client
  -> app global middleware (CORS, security, request ID, metrics, logger,
     recovery, audit, timeout, idempotency where wired)
  -> module route (JWT -> rate limit -> authz -> idempotency)
  -> handler -> service -> repository -> sqlc -> PostgreSQL
```

## Dependency rules

- A module must not import another module's internals. Use an exported
  contract in `internal/contracts` or a narrow interface.
- `internal/platform/*` and `pkg/*` must not import business modules.
- One module owns each table. Cross-module reads go through a read model or
  contract, never another module's repository.
- Generated sqlc code is shared infrastructure while the strangler migration
  is in progress; new modules should keep their queries and DTOs local.
- Enforced by `test/arch/imports_test.go` (`make arch`).

## Data and transactions

- One transaction scopes one module/aggregate. Do not span modules in a
  transaction; use jobs/events instead.
- The `jobs` table is the durable outbox. Enqueue with `Queue.EnqueueTx` so the
  job commits atomically with the business row.
- Email delivery is at-least-once. Handlers must be idempotent. Terminal jobs
  clear their payload and are purged after retention.
- Email payloads are encrypted at rest with AES-256-GCM; keys are derived from
  `JWT_SECRET` via HKDF. Rotate the secret only after draining the queue.

## Schema ownership

- Only `cmd/migrate` applies DDL. The API and seed assume the schema exists.
- Fresh installs use one baseline: `migrations/000001_init_schema.{up,down}.sql`.
- Once a database is deployed, add numbered migrations; never edit a shipped
  migration. Legacy databases must keep their recorded history (see README).

## Cross-cutting conventions

- **Authorization:** permissions live in `internal/platform/authz`; routes use
  `authz.Require`. Roles are an implementation detail of the permission matrix.
- **Audit:** successful mutating requests are recorded with actor, request ID,
  IP, and route. Explicit domain events use `audit.Recorder`.
- **Errors:** services return `*apperror.AppError` with a stable catalog code.
  HTTP renders RFC 9457 `application/problem+json`.
- **Idempotency:** opt-in `Idempotency-Key` on mutating routes, cache-backed,
  24h replay, body-bound for non-multipart requests.
- **Observability:** request IDs, structured `slog`, Prometheus HTTP/job/audit
  metrics, readiness checks for DB/cache.

## Strangler status

The module wrappers own route tables, but many handler/service/repository/DTO
internals still live in the legacy global packages. Moving one module's
internals at a time is the next phase; new code should start in
`internal/modules/<name>/` and use contracts for cross-module reads.

## Extension checklist

1. Create the module, tables, and queries; update the baseline for fresh
   installs (or add a numbered migration for deployed DBs).
2. Add permissions and audit actions.
3. Add handler/service/repository/DTO in the module directory.
4. Register routes and the module in `cmd/api/main.go`.
5. Add unit + integration tests; run `make check`, `go test -race ./...`, and
   `make swagger`.
6. For durable work, enqueue a job transactionally; never use `async.Go` for
   work that must survive a restart.
