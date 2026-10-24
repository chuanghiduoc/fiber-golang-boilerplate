# Features Module

Owns the tenant-scoped admin API for feature flags. Evaluation, the
compile-time catalog, the snapshot cache, and the database store live in
`internal/platform/features`; this module validates keys, writes database
defaults/targets, and invalidates the local snapshot.

## Routes

All routes are under `/api/v1/admin/features`, require JWT authentication, and
are rate limited. Reads use `admin:features:read`; writes use
`admin:features:write`, accept `Idempotency-Key`, and run behind a required
`audit.Guard`. `PUT`/`DELETE /flags/:key` change process-wide state and are
limited to platform-tenant (id 1) admins; other tenants manage only their own
targets.

| Method | Path | Handler |
| --- | --- | --- |
| GET | `/flags` | `ListFlags` |
| GET | `/flags/:key` | `GetFlag` |
| PUT | `/flags/:key` | `UpsertFlag` |
| DELETE | `/flags/:key` | `DeleteFlag` |
| GET | `/flags/:key/targets` | `ListTargets` |
| PUT | `/flags/:key/targets` | `UpsertTarget` |
| DELETE | `/flags/:key/targets/:id` | `DeleteTarget` |

## Tables

- `feature_flags` — process-wide `flag_key`, nullable `enabled` (NULL = no
  database default), `kill_switch`, `description`. Owned by
  `internal/platform/features`; this module writes it through its own sqlc
  queries for the admin API. Target upserts create a missing row with a NULL
  default.
- `feature_flag_targets` — tenant-owned `tenant_id` plus nullable `user_id`.
  The tenant always comes from the request context. A composite foreign key
  `(tenant_id, user_id) -> users(tenant_id, id)` prevents cross-tenant targets;
  `flag_key` references `feature_flags` with `ON DELETE RESTRICT`.

## Cross-module rules

This module must not import identity/files/admin internals. It uses only
`internal/platform/*`, `pkg/*`, and its own DTOs/repository/service/handler.
The platform evaluator is injected by `cmd/api`; the module does not own
evaluation logic.

## Tests

- `service/`: catalog merge, tenant scoping, description semantics, idempotent
  delete, platform-tenant restriction, NULL defaults, audit domain events,
  invalid-key 404s, and evaluator invalidation.
- `handler/`: request validation, response shapes, and errors.
- `module_test.go`: permission enforcement, route-level idempotency, and
  fail-closed audit guards.
- `test/integration/feature_flags_test.go`: SQL, tenant isolation, precedence,
  kill switch, and the full admin lifecycle.
