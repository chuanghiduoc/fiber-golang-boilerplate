# Features Module

Owns the platform and tenant back-office API for feature flags. Evaluation, the
compile-time catalog, the snapshot cache, and the database store live in
`internal/platform/features`; this module validates keys, writes database
defaults/targets, and invalidates the local snapshot.

## Routes

Process-wide flag rows live under `/api/v1/platform/feature-flags` and require
a `platform:features:*` permission, which implies a fresh second factor. A
tenant admin manages its own targets under `/api/v1/feature-flags` with
`admin:features:*`.

| Method | Path | Permission | Handler |
| --- | --- | --- | --- |
| GET | `/api/v1/platform/feature-flags` | `platform:features:read` | `listFeatureFlags` |
| GET | `/api/v1/platform/feature-flags/{key}` | `platform:features:read` | `getFeatureFlag` (ETag) |
| POST | `/api/v1/platform/feature-flags` | `platform:features:write` | `createFeatureFlag` (Tier 1) |
| PATCH | `/api/v1/platform/feature-flags/{key}` | `platform:features:write` | `updateFeatureFlag` (merge patch, Tier 1, If-Match) |
| DELETE | `/api/v1/platform/feature-flags/{key}` | `platform:features:write` | `deleteFeatureFlag` (Guard, If-Match) |
| GET | `/api/v1/platform/feature-flags/{key}/targets` | `platform:features:read` | `listFeatureFlagTargets` (all tenants) |
| PUT | `/api/v1/platform/feature-flags/{key}/targets/{targetId}` | `platform:features:write` | `putFeatureFlagTarget` |
| DELETE | `/api/v1/platform/feature-flags/{key}/targets/{targetId}` | `platform:features:write` | `deleteFeatureFlagTarget` |
| GET | `/api/v1/feature-flags` | `admin:features:read` | `listTenantFeatureFlags` (effective values) |
| GET | `/api/v1/feature-flags/{key}/targets` | `admin:features:read` | `listTenantFeatureFlagTargets` |
| PUT | `/api/v1/feature-flags/{key}/targets/{targetId}` | `admin:features:write` | `putTenantFeatureFlagTarget` |
| DELETE | `/api/v1/feature-flags/{key}/targets/{targetId}` | `admin:features:write` | `deleteTenantFeatureFlagTarget` |

Target ids are canonical item ids: `tenant:{tenantId}` / 
`user:{tenantId}:{userId}` on the platform routes, and `tenant` /
`user:{userId}` on the tenant routes. Platform target writes are cross-tenant
and authorize the process-wide permission before the repository uses the
row-level-security bypass for that one statement.

PATCH bodies use `application/merge-patch+json` (RFC 7396); `null` clears the
database default (`enabled`), the kill switch, or the custom description.
Flag create/update record `admin.feature_flag.updated` in the mutation
transaction (Tier 1); delete and target writes are Guarded.

## Tables

- `feature_flags` — process-wide `flag_key`, nullable `enabled` (NULL = no
  database default), `kill_switch`, `description`. Owned by
  `internal/platform/features`; this module writes it through its own sqlc
  queries for the admin API. Target upserts create a missing row with a NULL
  default.
- `feature_flag_targets` — tenant-owned `tenant_id` plus nullable `user_id`.
  The tenant comes from the request context, or from the target id on the
  platform route. A composite foreign key `(tenant_id, user_id) ->
  memberships(tenant_id, user_id)` prevents cross-tenant targets; `flag_key`
  references `feature_flags` with `ON DELETE RESTRICT`.

## Cross-module rules

This module must not import identity/files/admin internals. It uses only
`internal/platform/*`, `pkg/*`, and its own DTOs/repository/service/handler.
The platform evaluator is injected by `cmd/api`; the module does not own
evaluation logic.

## Tests

- `service/`: catalog merge, tenant scoping, description semantics, idempotent
  delete, platform restriction, NULL defaults, Tier 1 audit events, invalid-key
  404s, cross-tenant target authorization, and evaluator invalidation.
- `handler/`: request validation, response shapes, ETag/If-Match, merge-patch
  semantics, canonical target ids, and errors.
- `module_test.go`: permission and MFA enforcement, route-level idempotency, and
  fail-closed audit guards.
- `test/integration/feature_flags_test.go`: SQL, tenant isolation, precedence,
  kill switch, and the full admin lifecycle.
