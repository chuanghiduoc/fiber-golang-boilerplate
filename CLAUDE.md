# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
make build                  # Build binary (main.exe)
make run                    # Run locally (go run ./cmd/api)
make test                   # Run all tests (go test ./... -v)
make test-integration       # Integration tests (requires Docker, -tags=integration)
make lint                   # golangci-lint v2 (config: .golangci.yml)
make docker-run             # Docker Compose up (API + PostgreSQL + Redis + MinIO)
make docker-down            # Docker Compose down
make sqlc-generate          # Regenerate sqlc code in internal/sqlc/
make swagger                # Regenerate Swagger docs (swag init)
make seed                   # Seed admin user (go run ./cmd/seed)
make migrate-create name=x  # Create new migration pair
make watch                  # Live reload with Air
```

Run a single test file: `go test -v -run TestFunctionName ./internal/service/`

Production build: `CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o server ./cmd/api`

## Architecture

**Request flow**: `Client → Middleware → Handler → Service → Repository → sqlc → PostgreSQL`

Each layer depends only on the layer below via **interfaces** (constructor injection):

- **Handler** (`internal/handler/`): Parse HTTP request, validate, call service, return `pkg/response` JSON. Swagger annotations on each method.
- **Service** (`internal/service/`): Business logic. Accepts repository/cache/email interfaces. Returns `*apperror.AppError` or domain errors.
- **Repository** (`internal/repository/`): Wraps sqlc-generated code. Translates `pgx.ErrNoRows` → `apperror.ErrNotFound`. Each repo accepts `sqlc.DBTX` (works with both `*pgxpool.Pool` and `pgx.Tx`).
- **sqlc** (`internal/sqlc/`): **DO NOT EDIT** — generated from `queries/*.sql` + `migrations/*.sql` via `make sqlc-generate`. Config: `sqlc.yaml`.

**Dependency injection** wired manually in `cmd/api/main.go` — no DI framework.

## Error Handling

- Return `*apperror.AppError` from services/handlers — auto-handled by `apperror.FiberErrorHandler` in Fiber config.
- Constructors: `NewBadRequest`, `NewUnauthorized`, `NewForbidden`, `NewNotFound`, `NewInternal`, `NewValidation`.
- Sentinel: `apperror.ErrNotFound` — repositories return this for missing records, services check with `errors.Is(err, apperror.ErrNotFound)`.

## Response Format

Use `pkg/response` — never write raw JSON in handlers:
- `response.Success(c, data)` — 200
- `response.Created(c, data)` — 201
- `response.NoContent(c)` — 204
- `response.SuccessWithMeta(c, data, meta)` — 200 with pagination metadata

## Key Patterns

### Config
`config/config.go` — struct-based config parsed from env vars via `caarlos0/env`. Loaded once in main, passed by pointer. See `.env.example` for all options.

### Validation
`pkg/validator` wraps `go-playground/validator`. Custom `password` tag: 8–72 chars (bcrypt limit), must include upper + lower + digit + special.

### Roles
Constants in `internal/dto/role.go`: `dto.RoleUser`, `dto.RoleAdmin`. Use these instead of magic strings.

### Pagination
`dto.PaginationQuery` embedded in list request DTOs. Use `pkg/pagination.Normalize()`, `LimitOffset()`, `TotalPages()`.

### Transactions
`pkg/database.TxManager.WithTx(ctx, func(tx pgx.Tx) error { ... })` — pass `tx` to repository constructors inside the callback.

### Pluggable Drivers
Storage (`pkg/storage`), Cache (`pkg/cache`), Email (`pkg/email`) — each has an interface and factory function (`NewStorage`, `NewCache`, `NewSender`) that switches on config driver string (`local`/`s3`/`minio`, `memory`/`redis`, `console`/`smtp`).

### JWT
`pkg/token` — `Generate(userID, role, secret, expireHour)` and `Parse(tokenStr, secret)`. Includes `iss`/`aud` claims for cross-service protection.

### Safe Int Conversion
Use `pagination.clampInt32()` for `int → int32` casts. Never cast directly — gosec G115 is enabled globally to catch unsafe conversions.

### Rate Limiting
Tiered rate limiters in `internal/router/v1.go`: `strictLimiter` (auth endpoints), `normalLimiter` (mutations), `relaxedLimiter` (reads). Configured via `RATE_LIMIT_*` env vars.

### Soft Delete
Users and files use soft delete (`deleted_at` column). Partial indexes (`WHERE deleted_at IS NULL`) on frequently queried columns.

### Lint
golangci-lint v2 config at `.golangci.yml`. Excluded gosec rules: G101 (false positive on variable names), G304 (file paths from config). Generated code (`internal/sqlc/`, `docs/`) is excluded.

## Adding a New Entity

1. Create migration: `make migrate-create name=create_xxx_table`
2. Write SQL queries in `queries/xxx.sql` (with sqlc annotations: `-- name: Xxx :one`)
3. `make sqlc-generate`
4. Create DTO in `internal/dto/xxx_dto.go`
5. Create repository in `internal/repository/xxx_repository.go` (interface + impl)
6. Create service in `internal/service/xxx_service.go` (interface + impl)
7. Create handler in `internal/handler/xxx_handler.go` (with Swagger annotations)
8. Register routes in `internal/router/v1.go`
9. Wire DI in `cmd/api/main.go`
10. `make swagger`

## sqlc Workflow

Queries live in `queries/*.sql`. Migrations in `migrations/*.sql`. sqlc reads both to generate `internal/sqlc/`. After changing any SQL:

```bash
make sqlc-generate
```

Annotation format: `-- name: GetUserByID :one`, `-- name: ListUsers :many`, `-- name: CreateUser :exec`, etc.

## Testing

- Service tests in `internal/service/*_test.go` with mock implementations in `internal/service/mocks_test.go`.
- Package tests in `pkg/token/token_test.go`, `pkg/validator/validator_test.go`.
- Uses stdlib `testing` only — no testify dependency.
- Integration tests gated behind `-tags=integration` build tag.
