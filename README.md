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

Production-ready REST API boilerplate built with Go Fiber v3, PostgreSQL, and sqlc.

## Tech Stack

- **Framework**: [Fiber v3](https://docs.gofiber.io/) (Go 1.25+)
- **Database**: PostgreSQL 17 with [pgxpool](https://github.com/jackc/pgx)
- **Query**: [sqlc](https://sqlc.dev/) (type-safe SQL code generation)
- **Migration**: [golang-migrate](https://github.com/golang-migrate/migrate) (auto-run on startup)
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

```
cmd/api/main.go                     Entry point, DI, graceful shutdown
cmd/seed/main.go                    Standalone DB seeder
config/config.go                    Struct-based config from env vars (caarlos0/env)
internal/
  handler/                          HTTP handlers (parse request → call service → return response)
  service/                          Business logic (interfaces for testability)
  repository/                       Data access layer (wraps sqlc, error translation)
  dto/                              Request/Response structs + role constants
  sqlc/                             Generated code (DO NOT EDIT — use `make sqlc-generate`)
  middleware/                       JWT auth, rate limit, logger, recovery, security headers, metrics
  router/                           Route definitions, grouping, middleware wiring
  seed/                             Admin user seeder (idempotent)
pkg/
  apperror/                         AppError type + Fiber error handler + ErrNotFound sentinel
  response/                         Standardized JSON responses (Success, Created, NoContent, Error)
  database/                         PostgreSQL pool, auto-migration, TxManager
  validator/                        Struct validation (password: 8-72 chars, upper+lower+digit+special)
  token/                            JWT generation/parsing (iss/aud claims)
  cache/                            Cache interface (memory | redis)
  storage/                          Storage interface (local | s3 | minio)
  email/                            Email interface (console | smtp)
  pagination/                       Normalize, LimitOffset, TotalPages
  logger/                           slog setup (JSON in prod, text in dev)
  health/                           Liveness + readiness checks
  oauth/                            Google OAuth 2.0
  metrics/                          Prometheus HTTP metrics
  async/                            Fire-and-forget goroutine with panic recovery
migrations/                         SQL migration files (3 migrations: users, files, tokens)
queries/                            SQL query files for sqlc
docs/                               Generated Swagger docs
```

Request flow: `Client → Middleware → Handler → Service → Repository → sqlc → PostgreSQL`

## Quick Start

### With Docker (recommended)

```bash
cp .env.example .env
make docker-run
```

Services: API (:8080), PostgreSQL (:5432), Redis (:6379), MinIO (:9000/:9001)

### Without Docker

Prerequisites: Go 1.25+, PostgreSQL running locally.

```bash
cp .env.example .env
# Edit .env with your database credentials
make run
```

The app auto-runs migrations and seeds an admin user on startup.

## API Endpoints

### Auth (public)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/auth/register` | Register new user |
| POST | `/api/v1/auth/login` | Login, returns JWT + refresh token |
| POST | `/api/v1/auth/refresh` | Refresh access token |
| POST | `/api/v1/auth/logout` | Revoke refresh token |
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
| GET | `/api/v1/users/:id` | Get user by ID |
| GET | `/api/v1/users/` | List users (admin only) |
| PUT | `/api/v1/users/:id` | Update user (admin or self) |
| DELETE | `/api/v1/users/:id` | Delete user (admin or self) |

### Files (protected — JWT required)
| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/v1/files/upload` | Upload file |
| GET | `/api/v1/files/` | List own files (paginated) |
| GET | `/api/v1/files/:id` | Get file info |
| GET | `/api/v1/files/:id/download` | Download file |
| DELETE | `/api/v1/files/:id` | Delete file (soft) |

### Admin (protected — admin role required)
| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/admin/stats` | System statistics |
| GET | `/api/v1/admin/users` | List all users (including deleted) |
| PUT | `/api/v1/admin/users/:id/role` | Update user role |
| POST | `/api/v1/admin/users/:id/ban` | Ban user (soft delete) |
| POST | `/api/v1/admin/users/:id/unban` | Unban user (restore) |
| GET | `/api/v1/admin/files` | List all files |

### Infrastructure
| Method | Path | Description |
|--------|------|-------------|
| GET | `/healthz` | Liveness probe |
| GET | `/readyz` | Readiness probe (DB + cache) |
| GET | `/metrics` | Prometheus metrics |
| GET | `/swagger` | Swagger UI |

## Makefile Commands

```bash
make build                        # Build binary
make run                          # Run locally
make test                         # Run unit tests
make test-integration             # Run integration tests (requires Docker)
make lint                         # Run golangci-lint
make docker-run                   # Start with Docker Compose
make docker-down                  # Stop Docker Compose
make migrate-up                   # Run migrations manually
make migrate-down                 # Rollback migrations
make migrate-create name=xxx      # Create new migration
make sqlc-generate                # Regenerate sqlc code
make swagger                      # Regenerate Swagger docs
make seed                         # Seed database (admin user)
make watch                        # Live reload with Air
```

## Adding a New Entity

1. Create migration: `make migrate-create name=create_xxx_table`
2. Write SQL in `migrations/` (up + down)
3. Write queries in `queries/xxx.sql` (with sqlc annotations)
4. Run `make sqlc-generate`
5. Create `internal/dto/xxx_dto.go` (request/response structs)
6. Create `internal/repository/xxx_repository.go` (interface + impl wrapping sqlc)
7. Create `internal/service/xxx_service.go` (interface + impl with business logic)
8. Create `internal/handler/xxx_handler.go` (HTTP handler with Swagger annotations)
9. Register routes in `internal/router/v1.go`
10. Wire DI in `cmd/api/main.go`
11. Run `make swagger` to update docs

## Environment Variables

See [.env.example](.env.example) for all available configuration options with defaults.

Key settings:
- `APP_ENV` — `local` | `staging` | `production` (affects logging format, JWT secret validation)
- `REQUIRE_EMAIL_VERIFICATION` — Enable email verification requirement for login
- `STORAGE_DRIVER` — `local` | `s3` | `minio`
- `CACHE_DRIVER` — `memory` | `redis`
- `EMAIL_DRIVER` — `console` | `smtp`
- `ADMIN_EMAIL` / `ADMIN_PASSWORD` — Auto-seed admin user on startup
