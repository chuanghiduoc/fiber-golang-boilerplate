# Changelog

All notable changes to this project will be documented in this file.

Format based on [Keep a Changelog](https://keepachangelog.com/).

## [Unreleased]

Recreate local databases: the schema is one init migration edited in place
until the first release, and legacy access tokens must be re-issued.

### Breaking
- Users are global and join tenants through memberships; JWTs carry `sid`,
  `tenant_id`, `token_version`, and `amr`, and no longer a role (#120).
- Roles are database rows (`owner`, `admin`, `member` plus tenant roles);
  platform access is a `platform_admin` assignment (#120).
- Webhooks use the Standard Webhooks wire format and the versioned event
  envelope; producers publish domain events instead of calling webhooks
  (#116).
- Reusing an `Idempotency-Key` with a different body returns `422
  idempotency.key_mismatch` (#116).
- `migrations/` holds a single init pair with `-- owner:` sections; later
  changes are UTC-timestamped migrations (#120).
- The API, worker, and seed connect as a non-superuser role without
  `BYPASSRLS`; `cmd/migrate` uses the owner role via `DB_MIGRATE_*` (#120).

### Added
- Tenant-scoped RBAC with role management and platform tenant administration
  (`/admin/roles`, `PUT /admin/users/:id/roles`, `/platform/tenants`) (#120).
- TOTP MFA with recovery codes, two-step login, and `amr`; every `/platform`
  route requires MFA (#120).
- PostgreSQL row-level security on every tenant-owned table, with
  per-transaction scoping and explicit cross-tenant bypass (#120).
- GDPR data export and account erasure (`/me/data-exports`, `/me/erasure`,
  `/admin/users/:id/erasure`) (#120).
- Retention jobs for trashed files, read notifications, and expired exports;
  the trash purge claims rows before deleting blobs so a restore cannot revive
  a file without its blob (#118).
- CI API compatibility gate (`oasdiff breaking`) (#118).
- `cmd/gen` code generator and per-module sqlc configs (#114).
- Invitations and sessions with refresh-token reuse detection (#120).
- JWT and data-encryption key rotation, fail-closed service authorization,
  CODEOWNERS, and cosign image signing (#112).

### Changed
- Module services are framework-free and use their own types; the layering
  allowlist is empty (#113, #120, #121).
- Storage driver `minio` is replaced by `seaweedfs` (#106); PostgreSQL 18
  (#107) and Redis 8 (#108) are the supported versions.
- Dependabot waits 14 days before proposing a release (#119).

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
- Initial schema setup and admin seeding at startup (superseded by the one-shot `cmd/migrate` and `cmd/seed` commands)
- Swagger/OpenAPI docs
- Prometheus metrics, structured slog logging, health checks
- Tiered rate limiting (strict/normal/relaxed)
- CI pipeline (lint + test), Dockerfile multi-stage build
- GitHub issue templates, module rename script
- Unit tests for service layer, token, validator
