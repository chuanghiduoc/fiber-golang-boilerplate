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
cmd/api/main.go             bootstrap + module registration + graceful shutdown
cmd/migrate/main.go         the only schema initializer/migrator
cmd/seed/main.go            admin seeder (schema must exist)
internal/app/               Fiber app, global middleware, Module registry
internal/modules/           identity (+ admin seed), files (handler/service/repository), admin
internal/platform/          authz, audit, jobs, session, and future shared mechanics
internal/handler|service|repository|dto|sqlc   current internals (strangler phase)
pkg/                        apperror, response, database, cache, storage, email, ...
migrations/000001_init_schema.*   single consolidated up/down schema
deploy/k8s/                 Kubernetes manifests (see deploy/k8s/README.md)
```

**Request flow:** `Client → app middleware → module route → handler → service → repository → sqlc → PostgreSQL`.

**Design docs:** `docs/architecture.md` (module map, ownership, strangler
status), `docs/api-design.md` (API conventions, errors, pagination,
idempotency, compatibility), `docs/module-template.md` (canonical module
layout, scaffold, routes/permissions/tables/contracts), and
`docs/operations.md` (health, metrics, logs, jobs, idempotency, migrations,
Kubernetes, incidents). Keep them current when routes/contracts change.

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
Tenant isolation, per-module sqlc generation, distributed tracing, environment
promotion/rollout automation, and moving the remaining legacy
handler/service/repository/DTO internals into modules remain future work.
`files` already owns its upload handler/service; `identity` and `admin` still
wrap the global layers. Kubernetes manifests and a GHCR release workflow exist
under `deploy/k8s/` and `.github/workflows/release.yml`; they are a production
foundation, not a complete CD platform.

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
  failure rolls the token back. When transactional email provisioning is wired
  (as in `cmd/api/main.go`), registration is atomic too: `UserService.Register`
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
  failures that exhaust attempts, not lease-expiry dead letters. Email job
  payloads are sealed with AES-256-GCM using an HKDF-SHA256 key derived from
  `JWT_SECRET` (`jobs.PayloadCodec`); `EmailHandler` also accepts legacy
  plaintext JSON, which remains readable at rest until delivered or cleared.
  External tools see the envelope in `jobs.payload`. Encrypted jobs use the
  distinct kind `email.send.encrypted`; new workers register both kinds. During
  a rolling deploy old workers leave encrypted jobs queued (unknown-kind retry)
  instead of misreading them, so deploy the new worker to drain the queue.
  Rotating `JWT_SECRET` invalidates queued encrypted jobs, so drain the queue
  first.
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
- **Idempotency:** opt-in `Idempotency-Key` on mutating routes via
  `deps.Idempotency`; scoped by method/path/user (IP for public routes). 2xx
  responses replay for 24h; non-multipart bodies up to 1 MiB are body-bound, so
  reuse with a different body gets 409. An owner-token lock is held for 2m
  (compare-and-delete when the cache supports it), concurrent duplicates get
  409, and cache errors fail open. Replays set `Idempotent-Replayed: true` and
  `idempotency_replayed=true` so audit skips them. Keep `Idempotency-Key` in
  `CORS_ALLOW_HEADERS`; never add the middleware to token-minting endpoints or
  GET/HEAD/OPTIONS.

## Adding a Module

Run `scripts/new-module.sh <name>` and follow `docs/module-template.md`.

1. Create `internal/modules/<name>/module.go` implementing `app.Module`.
2. Keep handler/service/repository/DTO inside the module.
3. Add permissions and protect every route (legacy exceptions are documented
   in `docs/module-template.md`).
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
