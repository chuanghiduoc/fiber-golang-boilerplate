# Changelog

All notable changes to this project will be documented in this file.

Format based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Changed (API contract — breaking)
- Errors now follow RFC 9457 Problem Details (`application/problem+json`): `type`, `title`, `status`, `code` (snake_case i18n key), `detail`, `instance`, `requestId`, `timestamp`, and a flat `errors[]` for validation. Configurable docs base via `ERROR_DOCS_BASE_URL`.
- Successful responses return the resource directly (no `{data,meta}` envelope). List endpoints use the Stripe-style `{object:"list", url, data, hasMore}` envelope.
- List endpoints switched to forward-only cursor pagination (`limit` + `startingAfter`), backed by `(created_at, id)` indexes; the opaque cursor is `base64url(created_at:id)`. Offset helpers remain for small tables.
- JSON keys are camelCase; correlation header standardised to `X-Request-Id`.

### Security
- Hash password-reset and email-verification tokens (SHA-256) before storing, matching refresh tokens — a DB leak can no longer be replayed
- Upgrade SMTP connections via STARTTLS when advertised; add `SMTP_REQUIRE_TLS` to fail-close when the server offers no TLS
- Protect `/metrics` behind an optional `METRICS_AUTH_TOKEN` bearer (constant-time compare); drop the `Server` response header
- Add `APP_TRUST_PROXY`/`APP_TRUSTED_PROXIES` so `c.IP()` (rate-limit key) cannot be spoofed via `X-Forwarded-For`
- Make the login-attempt counter atomic (`Cache.Increment`) so parallel requests cannot bypass brute-force lockout
- Harden `Content-Disposition` to RFC 6266/5987 (strip control/path chars, encode Unicode via `filename*`)

### Fixed
- `TxManager.WithTx` now uses `errors.Join` on rollback failure so `errors.Is/As` still see the original business error
- Fix cross-platform path-traversal false positive: canonicalize the local storage base path (symlink/8.3 resolution)
- Readiness returns HTTP 503 when degraded; readiness checks now run truly in parallel
- `userService.Delete` and OAuth flows no longer swallow refresh-token revocation errors
- `token.Parse` returns an explicit error when the token is invalid; `email.NewSender` rejects unknown drivers
- `MemoryCache.Close` is idempotent (`sync.Once`); Redis `Increment` sets TTL atomically via a Lua script

### Improved
- Consolidate the schema into a single `000001_init_schema` migration with `CHECK` constraints on `role`/`auth_provider`
- Remove sqlc types from service APIs (`Authenticate`/`FindOrCreateByGoogle` return DTOs, `Download` returns a DTO, `Verify` returns the user ID) — cleaner layer boundary
- Validate DB pool sizing, cache/email drivers, and trusted-proxy configuration at startup

## [1.0.1] - 2026-02-28

### Fixed
- Log storage cleanup failure instead of silently ignoring in upload service
- Log cache errors in login lockout instead of silently ignoring
- Return email send error from SendVerification so callers can handle it
- Log refresh token revocation failure when banning users
- Fix down migration drop order (function before table)
- Fix password validator to use rune count for min length (Unicode support)

### Security
- Add Content-Security-Policy header to security middleware
- Add context-aware timeout (30s) to SMTP sender via net.DialContext
- Add production warnings in .env.example for CORS, DB_SSLMODE, JWT_SECRET, admin credentials

### Improved
- Mock repos now enforce email uniqueness, track email content, TTL, and deleted paths
- Add resource limits (memory/CPU) to all Docker Compose services
- Add Dependabot for Go modules, GitHub Actions, and Docker base images

## [1.0.0] - 2026-02-23

### Added
- Auth: register, login, refresh, logout, forgot/reset password, email verification, Google OAuth
- Users: CRUD, profile, change password, admin list
- Files: upload, download, list, soft delete (local/S3/MinIO storage)
- Admin: stats, user management (ban/unban/role), file listing
- JWT with iss/aud claims, refresh token rotation
- Pluggable drivers: storage (local/s3/minio), cache (memory/redis), email (console/smtp)
- Auto-migrations on startup, admin user seeding
- Swagger/OpenAPI docs
- Prometheus metrics, structured slog logging, health checks
- Tiered rate limiting (strict/normal/relaxed)
- CI pipeline (lint + test), Dockerfile multi-stage build
- GitHub issue templates, module rename script
- Unit tests for service layer, token, validator
