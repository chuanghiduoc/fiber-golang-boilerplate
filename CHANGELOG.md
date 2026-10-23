# Changelog

All notable changes to this project will be documented in this file.

Format based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

### Changed (API contract — breaking)
- Errors now follow RFC 9457 Problem Details (`application/problem+json`): `type`, `title`, `status`, `code` (snake_case i18n key), `detail`, `instance`, `requestId`, `timestamp`, and a flat `errors[]` for validation. Configurable docs base via `ERROR_DOCS_BASE_URL`.
- Successful responses return the resource directly (no `{data,meta}` envelope). List endpoints use the Stripe-style `{object:"list", url, data, hasMore, nextCursor}` envelope.
- List endpoints switched to forward-only cursor pagination (`limit` + `startingAfter`), backed by `(created_at, id)` indexes; the opaque cursor is `base64url(created_at:id)`. Offset helpers remain for small tables.
- JSON keys are camelCase; correlation header standardised to `X-Request-Id`.

### Security
- Consume refresh tokens atomically (`DELETE … RETURNING`) so a replayed/concurrent refresh cannot mint two sessions
- Bind refresh tokens to the authenticated token version and require a matching version at creation/consumption, closing the bcrypt/rotation race with password changes
- Add a `token_version` JWT claim plus a generation-keyed session cache: password changes/resets, bans, unbans, and role changes invalidate existing access tokens immediately without stale write-after-invalidate snapshots
- Track `password_changed_at` and reject reset links issued before the last password or email change
- Bind email-verification tokens to the address they were sent to and require the address to still match at verification time
- Add `POST /auth/logout-all` to revoke every refresh token and bump the token version, invalidating access tokens immediately
- Add admin file takedown (`DELETE /admin/files/:id`) and owner `includeDeleted=true` listing for trash/restore
- Normalize emails (trim + lowercase), enforce case-insensitive uniqueness among active users, and allow email reuse after soft delete
- Reset `email_verified_at` on email change, require the current password for self-service email changes, invalidate old verification tokens, and send a fresh link
- Revoke every refresh token and bump the token version on password change
- Remove the public `/uploads*` static route; files are only served through authenticated download endpoints, with a MIME-derived storage extension
- Require `verified_email=true` from Google and refuse to auto-link Google to an unverified local account
- Redact OAuth/token query parameters from request logs; validate client-supplied `X-Request-Id`
- Pin JWT parsing to HS256, require `exp`/`iat`, and add clock-skew leeway
- Reject self-demotion/self-ban and last-admin removal; bump token versions on role changes
- Protect the final admin and repair partially-seeded admins on startup
- Hash password-reset and email-verification tokens (SHA-256) before storing, matching refresh tokens — a DB leak can no longer be replayed
- Upgrade SMTP connections via STARTTLS when advertised; add `SMTP_REQUIRE_TLS` to fail-close when the server offers no TLS
- Protect `/metrics` behind an optional `METRICS_AUTH_TOKEN` bearer (constant-time compare); drop the `Server` response header
- Add `APP_TRUST_PROXY`/`APP_TRUSTED_PROXIES` so `c.IP()` (rate-limit key) cannot be spoofed via `X-Forwarded-For`
- Make the login-attempt counter atomic (`Cache.Increment`) so parallel requests cannot bypass brute-force lockout
- Harden `Content-Disposition` to RFC 6266/5987 (strip control/path chars, encode Unicode via `filename*`)

### Fixed
- `TxManager.WithTx` now always rolls back (including on panic), preventing pooled-connection leaks
- Password-reset requests are rate-limited before lookup and always return the same response, removing the account-enumeration oracle
- Email-verification tokens are single-use and email-bound; sibling password-reset tokens are invalidated after a successful reset
- Local uploads are written to a temporary file, size-checked, and atomically renamed; DB-failure cleanup uses a non-canceled context
- `APP_BODY_LIMIT` default now exceeds `STORAGE_MAX_FILE_SIZE`, so advertised 10 MB uploads are reachable
- DSN construction escapes credentials/database names; migrations resolve paths and schemes robustly
- Server exits non-zero when `Listen` fails; startup is bounded by a timeout; HTTP read/write/idle timeouts are configured
- Migration 000002 preserves email reuse after soft delete, creates real unique token-per-user indexes, and adds a DB trigger that makes last-admin removal safe under concurrency
- S3 downloads stat the object before responding and bound/cancel the stream; missing objects no longer surface after a 200
- Integration tests can run without Docker via `TEST_DATABASE_URL` using an isolated schema
- JSON bodies reject unknown fields and trailing values; duplicate emails return 409, oversized uploads 413, and unsupported media 415
- Non-owned user/file IDs return 404 instead of 403 so sequential IDs cannot be used to confirm another tenant's resources exist
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
