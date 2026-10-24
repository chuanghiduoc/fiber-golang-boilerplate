# Module Template

Canonical layout and checklist for a new business module in this modular
monolith. Read this with [Architecture](architecture.md) (dependency and
schema rules) and [Operations](operations.md) (runtime behavior).

Use the scaffold instead of copying directories by hand:

```bash
scripts/new-module.sh widgets
scripts/new-module.sh --dry-run widgets   # print paths, write nothing
```

The scaffold creates a compile-safe vertical slice and runs
`go build ./internal/modules/<name>/...` before reporting success. It
deliberately registers no routes; add them once the handler methods exist.
`<name>` must be lower_snake_case; the script rejects Go keywords, reserved
package names, and Go-reserved directories such as `vendor` and `testdata`. Do
not commit a probe module created only to test the script.

## 1. Canonical layout

```text
internal/modules/<name>/
├── module.go              package <name>: implements app.Module, owns the route table
├── README.md              ownership, routes, permissions, tables, jobs, contracts
├── dto/
│   └── dto.go             request/response transport types; no HTTP or SQL
├── handler/
│   └── handler.go         bind/validate HTTP, call service, write responses
├── repository/
│   └── repository.go      SQL/sqlc, transactions, row mapping; no HTTP
└── service/
    └── service.go         business rules, ownership checks, orchestration
```

Related repository-level files:

```text
queries/<name>.sql                         sqlc source for this module's tables
migrations/NNNNNN_<name>_<purpose>.up.sql  additive schema for a deployed database
migrations/NNNNNN_<name>_<purpose>.down.sql
```

Responsibilities and import direction:

| Package | Owns | May import | Must not import |
| --- | --- | --- | --- |
| `module.go` | module identity and route table | `internal/app`, own subpackages, `platform`, `pkg` | another module |
| `handler` | HTTP binding, validation, status/response shape | own `dto`, own `service`, `platform/httpx`, `pkg` | own `repository`, SQL |
| `service` | business rules, authorization decisions, transactions, jobs | own `dto`, own `repository` interface, `platform`, `pkg` | Fiber handlers |
| `repository` | SQL, sqlc rows, persistence errors | own `dto`/domain types, `pkg/database`, shared `internal/sqlc` | own `handler`, Fiber |
| `dto` | transport structs, validation tags | stdlib only (or `pkg/pagination` for shared query types) | repository, sqlc, Fiber |

Keep the module root small. If a module needs several vertical slices (for
example `orders` and `invoices`), prefer separate modules over a large shared
package.

## 2. Scaffold skeleton

The generated files are intentionally minimal. Replace the `Ping` placeholders
with real use cases; keep the package boundaries.

`internal/modules/widgets/module.go`:

```go
// Package widgets owns the widgets business capability.
//
// It implements app.Module and owns this module's route table. Keep business
// rules in service, persistence in repository, and HTTP in handler.
package widgets

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	widgetshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/widgets/handler"
)

// Module wires the widgets HTTP surface into the application.
type Module struct {
	handler *widgetshandler.Handler
}

// New builds the module from its handler. Construct the handler, service, and
// repository in cmd/api/main.go.
func New(handler *widgetshandler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return "widgets" }

// RegisterRoutes mounts routes below /api/v1. The scaffold is intentionally
// empty; copy the patterns below when the handlers are ready.
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
}
```

`internal/modules/widgets/handler/handler.go`:

```go
// Package handler owns HTTP transport for the widgets module.
package handler

import (
	"github.com/gofiber/fiber/v3"

	widgetsdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/widgets/dto"
	widgetsservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/widgets/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// Handler binds requests, validates input, and calls the service.
type Handler struct {
	service *widgetsservice.Service
}

// New returns a handler backed by service.
func New(service *widgetsservice.Service) *Handler {
	return &Handler{service: service}
}

// Health is a placeholder used by the scaffold; replace it with real handlers.
func (h *Handler) Health(c fiber.Ctx) error {
	if err := h.service.Ping(c.Context()); err != nil {
		return err
	}
	return response.Success(c, widgetsdto.HealthResponse{
		Module: "widgets",
		Status: "ok",
	})
}
```

`internal/modules/widgets/service/service.go`:

```go
// Package service owns business rules for the widgets module.
package service

import "context"

// Repository is the persistence surface this service needs. Keep the
// interface narrow and declare it here so tests can replace it.
type Repository interface {
	Ping(ctx context.Context) error
}

// Service coordinates validation, transactions, and repository calls.
type Service struct {
	repository Repository
}

// New returns a service backed by repository.
func New(repository Repository) *Service {
	return &Service{repository: repository}
}

// Ping is a placeholder; replace it with real use cases.
func (s *Service) Ping(ctx context.Context) error {
	return s.repository.Ping(ctx)
}
```

`internal/modules/widgets/repository/repository.go`:

```go
// Package repository owns persistence for the widgets module.
package repository

import "context"

// Repository is the module's PostgreSQL access point. Keep SQL and row
// mapping here; handlers and services must not reach the database directly.
type Repository struct{}

// New returns a repository. Replace the empty struct with the pgx pool and,
// once added, the module's sqlc queries.
func New() *Repository {
	return &Repository{}
}

// Ping is a placeholder; remove it when the module has real queries.
func (r *Repository) Ping(context.Context) error {
	return nil
}
```

`internal/modules/widgets/dto/dto.go`:

```go
// Package dto contains the widgets module's transport types.
package dto

// Item is a placeholder response; replace it with the module's real types.
type Item struct {
	ID int64 `json:"id"`
}

// CreateItemRequest is a placeholder request with validation.
type CreateItemRequest struct {
	Name string `json:"name" validate:"required,max=255"`
}

// HealthResponse is the scaffold's placeholder health response.
type HealthResponse struct {
	Module string `json:"module"`
	Status string `json:"status"`
}
```

Generated `README.md` records ownership and next steps. Keep it current; it is
the first file the next team reads.

## 3. Route wiring

Routes are mounted by `app.Register` below `/api/v1`. A module must never mount
global middleware or another module's routes.

Recommended order for a protected route:

```text
JWT auth (group) -> rate limit (tier) -> authz.Require -> idempotency -> handler
```

- `JWTAuth` belongs on the module group (or per route for mixed public/private
  groups), not on the root app.
- Use `deps.Config` for secrets and `deps.Sessions` for session validation;
  never capture a second config copy.
- Use the tier that matches cost: `Strict` for credential/token endpoints,
  `Normal` for mutations and uploads, `Relaxed` for reads.
- Protect every route with a permission, even reads. Public routes are an
  explicit product decision and still get a rate limit.
- Add `deps.Idempotency` only to mutating routes that are safe to replay. Never
  add it to login, refresh, logout, forgot/reset/verify, or OAuth callbacks.
- Return `*apperror.AppError` from handlers and services; use
  `pkg/response` helpers instead of raw `c.JSON` for API responses.
- Keep route paths static. Metrics and audit use `c.Route().Path` as the route
  template; generated paths create high-cardinality labels and a poor audit
  trail.

Reference route table:

```go
func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	group := v1.Group("/widgets",
		middleware.JWTAuth(deps.Config.JWT.Secret, deps.Sessions),
	)

	group.Get("/",
		deps.Limiters.Relaxed,
		authz.Require(authz.PermissionWidgetsRead),
		m.handler.List,
	)
	group.Get("/:id",
		deps.Limiters.Relaxed,
		authz.Require(authz.PermissionWidgetsRead),
		m.handler.Get,
	)
	group.Post("/",
		deps.Limiters.Normal,
		authz.Require(authz.PermissionWidgetsWrite),
		deps.Idempotency,
		m.handler.Create,
	)
	group.Put("/:id",
		deps.Limiters.Normal,
		authz.Require(authz.PermissionWidgetsWrite),
		deps.Idempotency,
		m.handler.Update,
	)
	group.Delete("/:id",
		deps.Limiters.Normal,
		authz.Require(authz.PermissionWidgetsDelete),
		deps.Idempotency,
		m.handler.Delete,
	)
}
```

For a public group, omit `JWTAuth` and use `Strict` where credentials are
involved. For an admin-only sub-surface, mount it under the existing admin
group only after the owner agrees; otherwise expose a dedicated permission and
keep the module's routes together.

### Legacy exceptions (do not copy)

The current tree predates the target rules. These are documented exceptions,
not the convention for new modules:

- **`files` authorization:** its routes use JWT and rate limits but do not call
  `authz.Require`. Target: every route has a permission, including reads.
- **`identity` user reads/updates:** `GET /users/:id` and some update paths use
  owner/admin checks in the handler (`authRole`/`authUserID`) rather than route
  permissions. Target: permission middleware, with ownership checks in the
  service only for row-level access.
- **`files.user_id`:** the existing foreign key uses `ON DELETE CASCADE` to
  `users`, so a hard delete of a user removes file metadata across a module
  boundary. Target: no cross-module foreign key or cascade; core identity
  references require explicit owner approval. Keep the existing behavior until
  a reviewed migration changes it.

## 4. Permissions

Permissions live in `internal/platform/authz/permissions.go`. One permission
is one stable capability:

```go
const (
	PermissionWidgetsRead   Permission = "widgets:read"
	PermissionWidgetsWrite  Permission = "widgets:write"
	PermissionWidgetsDelete Permission = "widgets:delete"
)
```

Add every constant to `allPermissions` (admins receive all entries by
construction) and grant non-admin roles deliberately in `buildRolePermissions`.
Do not reuse another module's permission, and do not authorize by role string
in a handler. Unknown roles have no permissions.

Permission checklist:

1. Add constants and include them in `allPermissions`.
2. Add the intended grants to `buildRolePermissions` (usually not to `user`).
3. Protect every route with `authz.Require` (or `RequireAny` for a documented
   union).
4. Add a table test for the role matrix.
5. Mention the permission names in the module README.

## 5. Audit

The global `platform/audit.Middleware` already records every successful
mutating request (POST/PUT/PATCH/DELETE) with actor, route template, request
ID, IP, user agent, and status. Its `Action` is the generic
`<METHOD> <route template>` value, for example `POST /api/v1/widgets/`;
idempotent replays are not recorded twice.

Use an explicit domain event only when the generic request record is not enough
(for example an asynchronous side effect or a privileged cross-resource action).
Explicit events use `<module>.<resource>.<verb>` actions such as
`widgets.widget.published`:

```go
event := audit.Event{
	Action:     "widgets.widget.published",
	Resource:   "widget",
	ResourceID: strconv.FormatInt(widget.ID, 10),
	Metadata:   map[string]any{"ownerId": ownerID},
}
ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
defer cancel()
if err := recorder.Record(ctx, event); err != nil {
	metrics.AuditWriteFailuresTotal.Inc()
	slog.Warn("audit write failed", "action", event.Action, "error", err)
}
```

`recorder` is the `audit.Recorder` injected into the service (use
`audit.Noop{}` in tests). `Recorder.Record` only persists the event; it does
not increment the metric or log. The generic middleware does both, so an
explicit caller must increment `metrics.AuditWriteFailuresTotal` and log its
own failure as shown.

Rules:

- The generic middleware action format is fixed as `<METHOD> <route template>`.
  Explicit domain events use `<module>.<resource>.<verb>`, not prose.
- Metadata must be JSON-marshalable and must never contain passwords, tokens,
  email bodies, reset links, or full request bodies.
- Audit is best-effort at the request boundary: a write failure is logged (and
  counted in `audit_write_failures_total` by the generic middleware) but does
  not fail the business request. Do not silently drop an audit failure.
- Do not record secrets just because the event is important. Record identifiers
  and outcomes.
- The audit table is append-only in normal operation; do not add update/delete
  paths for audit rows.

## 6. Idempotency

`deps.Idempotency` is opt-in per route. When a client sends
`Idempotency-Key`:

- the first 2xx response (status, body, `Content-Type`, `Location`, `ETag`) is
  cached for 24 hours; retries replay it with `Idempotent-Replayed: true`;
- non-multipart request bodies up to 1 MiB are SHA-256-bound, so the same key
  with a different body returns `409 Conflict`;
- the owner holds a 2-minute lock; concurrent duplicates return `409`;
- cache read/write failures fail open, so the handler still runs normally.

Apply it to create/update/delete/restore-style routes. Do not apply it to
token-minting or credential-verification endpoints, GET/HEAD/OPTIONS, or routes
whose side effect is not safe to repeat. For multipart uploads, remember the
body is not bound: reuse the same key only for the same file operation.

Use Redis (`CACHE_DRIVER=redis`) in any multi-replica deployment. Memory cache
is per process, so a retry that lands on another replica executes again. See
[Operations](operations.md#5-idempotency-and-redis) for TTLs, outage behavior,
and cleanup.

## 7. Tables, queries, and migrations

### Ownership

One module owns each table. Current owners:

| Owner | Tables |
| --- | --- |
| `identity` | `users`, `refresh_tokens`, `password_reset_tokens`, `email_verification_tokens` |
| `files` | `files` |
| `platform/jobs` | `jobs` |
| `platform/audit` | `audit_events` |

New modules own tables named after the module or with a `<name>_` prefix (for
example `widgets`, `widgets_events`).
Do not read or write another module's tables directly, even in a JOIN. Use a
contract/read model or a job.

### Schema conventions

- Primary keys are `BIGSERIAL` unless there is a strong reason otherwise.
- `created_at` and `updated_at` are `TIMESTAMP WITH TIME ZONE NOT NULL`; use
  the shared `update_updated_at()` trigger for mutable tables.
- Soft delete uses `deleted_at` plus partial unique indexes for active rows.
- Cursor pagination uses a `(created_at DESC, id DESC)` index.
- Foreign keys inside the module are fine. Avoid cross-module foreign keys; if
  a core reference (for example `user_id`) is unavoidable, get the owner's
  agreement, document it, and never cascade across the boundary. The existing
  `files.user_id ... ON DELETE CASCADE` is a legacy exception, not a template.
- Index every ownership and lookup path used by list/detail endpoints. Add
  indexes in the same migration as the table for a new module.

### Migration rules

- Only `cmd/migrate` applies DDL. The API and seed commands assume the schema
  exists.
- **Upstream boilerplate before its first deployment:** extend the single
  `migrations/000001_init_schema.{up,down}.sql`.
- **Any deployed database or downstream fork:** never edit `000001`. Add the
  next numbered migration, for example
  `000002_widgets_init.{up,down}.sql` (or `000004+` when upgrading a legacy
  history). Never reuse or renumber a shipped migration.
- A database that applied the old `000001`–`000003` history keeps that history.
  Restore those source files from git history if needed and add `000004+`; do
  not point it at the consolidated baseline. See
  [Upgrading an existing database](../README.md#upgrading-an-existing-database).
- The down migration reverses only what its up migration created. Do not use
  down migrations as the production rollback plan; they are for local/test
  reset and reviewed emergency use.
- Keep each migration short. Backfills, reindexes, and large data rewrites are
  jobs, not DDL transactions. Use expand/contract for incompatible column
  changes and make the API tolerate both shapes during rollout.
- Add indexes for a live table with `CREATE INDEX CONCURRENTLY` when the
  migration runner permits it; otherwise schedule a maintenance window and
  document the lock.
- Test the migration on a copy of a production-shaped database, and take a
  verified backup/PITR snapshot before applying it. A migration is not a
  backup.

Example table:

```sql
CREATE TABLE IF NOT EXISTS widgets (
    id BIGSERIAL PRIMARY KEY,
    -- identity user ID; intentionally no cross-module foreign key
    owner_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_widgets_owner_created
    ON widgets (owner_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL;

DROP TRIGGER IF EXISTS trigger_widgets_updated_at ON widgets;
CREATE TRIGGER trigger_widgets_updated_at
    BEFORE UPDATE ON widgets
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at();
```

### Queries and sqlc

Put SQL in `queries/<name>.sql` and run `make sqlc-generate`. Today generated
code lands in the shared `internal/sqlc` package; keep query names prefixed
(`ListWidgetsByOwner`, `CreateWidget`) and never import another module's
generated types. If per-module sqlc generation is introduced later, this layout
already isolates the source. SQLC generation is not a substitute for a
migration: the schema must be applied first.

## 8. Cross-module contracts

A module never imports another module's package, including its service or
repository. When module B needs data or behavior from module A, define a
contract package outside both modules:

```text
internal/contracts/identity.go
```

```go
package contracts

import "context"

// User is the contract view of an identity user. Do not add transport or SQL
// types here.
type User struct {
	ID    int64
	Email string
	Role  string
}

// UserReader is implemented by the identity module or a small adapter in the
// composition root. Keep methods additive and version a breaking change.
type UserReader interface {
	GetUser(ctx context.Context, id int64) (User, error)
}
```

Rules:

- The contract package must not import `internal/modules/*`, Fiber, or sqlc.
  Contracts in `internal/contracts` are stdlib-only; they also must not import
  repositories, DTOs, storage, database, or session packages.
- The provider implements the interface structurally; the consumer depends on
  the interface, not the implementation.
- Wire the implementation in `cmd/api/main.go`. The composition root is the
  only place that knows both sides.
- A contract exposes identifiers and stable values, not repositories, database
  rows, or DTOs from the provider's HTTP layer.
- Errors are part of the contract. Map them to `apperror` at the transport
  boundary; do not leak SQL errors or provider internals.
- Adding a method to an interface is a breaking change for implementers.
  Prefer a new, narrowly named contract for a new capability.
- For side effects across modules, enqueue a durable job instead of calling
  another module's service inside a transaction. The `jobs` table is the
  outbox; use `Queue.EnqueueTx` when the event must commit with the business
  row.

If a module only needs a read model and a contract would be awkward, own a
local read-only table populated by a job, or a narrow query owned by the data
owner. Never reach into another module's repository.

## 9. What must not be imported

| From | Forbidden |
| --- | --- |
| Any `internal/modules/<a>` | `internal/modules/<b>` (any package) |
| `internal/platform/*`, `pkg/*` | any `internal/modules/*` |
| A module handler | its own repository, another module, Fiber global setup |
| A module service | Fiber context/handlers, another module |
| `internal/contracts/*` | any module implementation, repository, DTO, sqlc, storage, database, session, or Fiber |
| `cmd/api` | module internals other than exported constructors |

`test/arch/imports_test.go` fails the build on cross-module and
platform/pkg-to-module imports. Run `make arch` after adding a module. Do not
weaken the test to make a shortcut compile; introduce a contract instead.

## 10. Tests

Minimum for a new module:

- `module_test.go` (or a small package test): `Name()` is stable and the module
  satisfies `app.Module`.
- Handler tests: construct a Fiber app with the handler and a fake service;
  assert status, `Content-Type`, problem details, and permission behavior.
- Service tests: fake the narrow repository interface; cover ownership checks,
  validation, error mapping, and any job enqueue path.
- Repository integration test with `-tags=integration` and
  `internal/testutil.SetupTestDB` (Testcontainers or `TEST_DATABASE_URL`).
  Cover pagination, soft delete/restore, uniqueness, and transaction rollback.
- Architecture test: `make arch`.

A handler test should not need PostgreSQL:

```go
app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
app.Post("/api/v1/widgets", handler.Create)

req, _ := http.NewRequest(http.MethodPost, "/api/v1/widgets", strings.NewReader(`{"name":"demo"}`))
req.Header.Set("Content-Type", "application/json")
resp, err := app.Test(req)
require.NoError(t, err)
assert.Equal(t, fiber.StatusCreated, resp.StatusCode)
```

For full-stack route tests, construct `app.New` with a test config and register
the module, as `internal/app/app_test.go` does. Use unique names/IDs per test
and never depend on a shared production database.

Run before opening a PR:

```bash
go fmt ./internal/modules/<name>/...
make arch
make check
go test -race ./...
make swagger   # if route annotations changed
```

## 11. Register in the composition root

Construct dependencies in `cmd/api/main.go`, then register the module once:

```go
widgetRepo := widgetsrepository.New()
widgetService := widgetsservice.New(widgetRepo)
widgetHandler := widgetshandler.New(widgetService)

application := app.New(cfg, app.Deps{
	Cache:    appCache,
	Sessions: sessionStore,
	Health:   healthChecker,
	Audit:    audit.NewPostgresRecorder(pool),
})
application.Register(
	identity.New(authHandler, userHandler),
	files.New(uploadHandler),
	admin.New(adminHandler),
	widgets.New(widgetHandler),
)
```

Rules:

- The scaffold's generated `repository.New()` takes no arguments. When you
  replace its empty struct with the pgx pool, change it to `New(pool)` and
  update this call; keep construction in the composition root.
- Keep module-specific dependencies in the module constructor; do not grow
  `app.Deps` for one module.
- Register before `Listen`. Duplicate paths are a startup/programming error.
- If the module owns durable work, register its job handler before
  `worker.Run`, for example `worker.Register("widgets.reindex", widgetWorker)`.
  Job kinds use a `<module>.<action>` prefix.
- Startup order matters: database -> migrations (separate command) -> storage ->
  cache -> email -> repositories -> services -> handlers -> app -> worker.
- Do not enqueue work during `RegisterRoutes`; request handling must be the
  trigger, or an explicit startup backfill job.

## 12. Add-a-module checklist

1. `scripts/new-module.sh <name>` (or create the same layout by hand). Keep the
   generated README and fill in ownership.
2. Add permissions to `internal/platform/authz` and a role-matrix test.
3. Add the migration (baseline only for an undeployed upstream; otherwise the
   next numbered migration), queries, and `make sqlc-generate`.
4. Implement repository -> service -> handler. Keep transport, rules, and SQL
   in their packages.
5. Add routes to `module.go`: JWT, rate limit, permission, idempotency where
   appropriate, then handler.
6. Wire dependencies and register the module in `cmd/api/main.go`; register
   job handlers if any.
7. Add unit, handler, and integration tests; run `make arch`.
8. Update the module README, `docs/architecture.md` if the module map changes,
   and Swagger annotations if routes were added.
9. Run `gofmt`, `make check`, `go test -race ./...`, and `make swagger`.
10. Confirm no probe module, generated binary, or secret is staged. One module
    per commit is preferred.

## 13. Conventions for many modules

- **Names:** lower_snake_case directories; `<name>_` tables; `<name>:read` /
  `<name>:write` permissions; `<name>.<action>` job kinds;
  `<name>.<resource>.<verb>` audit actions. Prefixes make ownership obvious at
  scale.
- **One reason to change:** avoid shared "common" or "utils" domain packages.
  Shared mechanics belong in `platform`/`pkg`; shared business concepts belong
  in contracts.
- **Small interfaces:** define repository and contract interfaces at the
  consumer, keep them narrow, and fake them in tests.
- **No cross-module joins:** a read model or contract is cheaper than an
  ownership dispute at 100 modules.
- **Durable side effects:** enqueue after/with the owning transaction; never
  use `async.Go` for work that must survive a restart.
- **Observability:** one permission namespace, one job-kind prefix, and one
  bounded route table per module keep dashboards and alerts usable.
- **Documentation:** the module README is part of the module. Record tables,
  routes, permissions, jobs, contracts, and any operational caveat there.
