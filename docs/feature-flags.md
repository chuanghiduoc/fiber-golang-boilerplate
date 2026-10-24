# Feature Flags

Feature flags are compile-time keys with optional PostgreSQL state, tenant/user
targets, and an in-process snapshot cache. They are owned by
`internal/platform/features` (catalog, evaluator, database store, metrics) and
administered through `internal/modules/features` (HTTP + sqlc).

The feature is deliberately dependency-free: no flag provider SDK, no external
service, and no new module-to-module imports.

## Catalog

Only flags declared in `internal/platform/features/catalog.go` exist. Add a
`Key` constant and a `Definition` before referencing the flag from environment
configuration or the admin API. Unknown keys fail startup validation and
return `404` from the admin API, so a typo cannot silently disable a feature.

Current catalog:

| Key | Compiled default | Purpose |
| --- | --- | --- |
| `user_registration` | `true` | Allow public registration. |
| `email_verification` | `false` | Require email verification before authenticated actions. |
| `google_oauth` | `false` | Expose Google OAuth login/callback. |
| `file_uploads` | `true` | Allow authenticated file uploads. |

These defaults describe the baseline behavior; using a flag to change a route
is a product decision made by the owning module. The evaluator is available to
every module through `app.Deps.Flags`:

```go
enabled := deps.Flags.Enabled(
    c.Context(),
    features.KeyFileUploads,
    features.SubjectFromContext(c.Context(), httpx.AuthUserID(c)),
)
```

`app.New` replaces a nil `Deps.Flags` with an evaluator backed by environment
values, force-off, and compiled defaults, so modules never need a nil check.

## Precedence

Evaluation stops at the first matching source, highest first:

1. **Force-off** — `FEATURE_FLAGS_FORCE_OFF` contains the key. Always `false`.
2. **Kill switch** — `feature_flags.kill_switch` is true. Always `false`, but
   the database row (and targets) remain for a later re-enable.
3. **User target** — `feature_flag_targets` row for `(flag, tenant, user)`.
4. **Tenant target** — `feature_flag_targets` row for `(flag, tenant)` with
   `user_id IS NULL`.
5. **Database default** — `feature_flags.enabled`.
6. **Environment** — `FEATURE_FLAGS` assignment.
7. **Compiled default** — `Definition.Default`.

A target can be `true` or `false`; both override lower levels. Tenant/user
lookups use the request tenant resolved by `tenant.Middleware` (and the JWT
claim for authenticated routes). A user target is keyed by both tenant and
user, so the same user ID in another tenant does not inherit it.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `FEATURE_FLAGS` | empty | Comma-separated `key=true\|false` assignments; a bare key means true, e.g. `user_registration=false,file_uploads`. |
| `FEATURE_FLAGS_FORCE_OFF` | empty | Comma-separated keys that always evaluate false. |
| `FEATURE_FLAGS_CACHE_TTL_SECONDS` | `30` | How long a loaded database snapshot is fresh. |
| `FEATURE_FLAGS_MAX_STALENESS_SECONDS` | `300` | Maximum age of the last good snapshot when the database is unavailable. Never below the TTL. |

Config validation rejects malformed assignments, duplicate keys, unknown
catalog keys, negative timings, and `MAX_STALENESS < CACHE_TTL`. Zero timings
fall back to the defaults (useful for tests that build `Config` literals).

## Database

Both tables are created by the fresh-install baseline `000001_init_schema`:

- `feature_flags` is process-wide configuration: `flag_key` primary key,
  `enabled` (database default), `kill_switch`, `description`, timestamps.
- `feature_flag_targets` is tenant-owned: `flag_key`, `tenant_id`, nullable
  `user_id`, `enabled`, timestamps. A `(tenant_id, user_id)` composite foreign
  key references `users(tenant_id, id)`, so a target cannot point at a user in
  another tenant. Deleting a flag cascades to its targets.

Partial unique indexes allow one tenant-wide target and one target per user per
flag:

- `idx_feature_flag_targets_tenant_target` on `(flag_key, tenant_id)` where
  `user_id IS NULL`;
- `idx_feature_flag_targets_user_target` on `(flag_key, tenant_id, user_id)`
  where `user_id IS NOT NULL`.

`cmd/migrate` verifies both tables, their tenant/key columns, and these unique
indexes (plus `users(tenant_id, id)`, which backs the composite foreign key).

## Cache and invalidation

The evaluator keeps one full snapshot in process memory:

- A snapshot younger than `FEATURE_FLAGS_CACHE_TTL_SECONDS` is served without a
  database query.
- An expired snapshot triggers one reload; concurrent callers serialize behind
  the refresh lock and reuse the fresh result.
- If a reload fails, the last good snapshot is served only while it is no older
  than `FEATURE_FLAGS_MAX_STALENESS_SECONDS`. After that, evaluation falls back
  to force-off/environment/compiled defaults. Failed reloads are throttled for
  one TTL so an outage does not cause a query on every request.
- The admin service calls `Evaluator.Invalidate` after every successful write,
  so the writing replica sees the change immediately. Other replicas converge
  within the cache TTL; there is no pub/sub dependency.

Force-off is checked before any snapshot lookup, so an operator can disable a
flag even while the database is unavailable.

## Admin API

All routes require a JWT and are mounted under `/api/v1/admin/features`.
`admin:features:read` guards reads; `admin:features:write` guards mutations.
Mutating routes accept `Idempotency-Key` through the normal middleware.

| Method | Path | Description |
| --- | --- | --- |
| GET | `/flags` | List every catalog flag with database default, kill switch, configured state, and the caller's effective value/source. |
| GET | `/flags/{key}` | Flag plus tenant targets. |
| PUT | `/flags/{key}` | Upsert the database default, kill switch, and description (description defaults to the existing value or catalog description). |
| DELETE | `/flags/{key}` | Delete the database row and cascade its targets; the flag reverts to env/compiled defaults. Idempotent for a valid catalog key. |
| GET | `/flags/{key}/targets` | List tenant-wide and user targets in the request tenant. |
| PUT | `/flags/{key}/targets` | Upsert a target. Omit `userId` for a tenant-wide target; include it for a user target. The tenant always comes from context, never the body. |
| DELETE | `/flags/{key}/targets/{id}` | Delete a target by ID, scoped to the request tenant and flag key. |

`enabled` and `killSwitch` are required on `PUT /flags/{key}`; `enabled` is
required on `PUT .../targets`. Requests reject unknown JSON fields. A user
target whose user is not in the request tenant returns `400` (composite FK).

Flag definitions are process-wide; targets are tenant-scoped. An administrator
of any tenant can change the global default/kill switch because deployments
are expected to treat back-office admins as trusted operators. Use
tenant/user targets for per-tenant rollout.

Successful mutations enrich the generic audit event with non-secret metadata:
`feature`, `enabled`, `killSwitch`, `source`, and, for targets, `targetId` and
`userId`. The flag key is also used as the audit resource ID.

## Metrics

| Metric | Labels | Meaning |
| --- | --- | --- |
| `feature_flag_evaluations_total` | `key`, `source`, `enabled` | Evaluation results by precedence source. |
| `feature_flag_snapshot_loads_total` | `result` | Database snapshot loads (`success`/`error`). |
| `feature_flag_cache_hits_total` | — | Evaluations served from a fresh snapshot. |
| `feature_flag_cache_misses_total` | — | Cold/expired snapshot evaluations. |
| `feature_flag_snapshot_stale_total` | — | Evaluations served from a stale snapshot during a database outage. |
| `feature_flag_snapshot_fallback_total` | — | Evaluations that fell back to env/compiled defaults. |
| `feature_flag_snapshot_age_seconds` | — | Age of the snapshot currently used. |
| `feature_flag_invalidations_total` | — | Local snapshot invalidations. |

Alert on sustained `feature_flag_snapshot_loads_total{result="error"}` or a
rising `feature_flag_snapshot_fallback_total`: both mean the evaluator is
serving defaults instead of database state.

## Operations

- To stop a misbehaving feature immediately, set
  `FEATURE_FLAGS_FORCE_OFF=<key>` and restart the process (or set the flag's
  kill switch through the admin API). Force-off does not depend on the database.
- To roll out by tenant, leave the database default off and add tenant/user
  targets.
- Rotating or deleting a flag row is safe: evaluation falls back to environment
  and compiled defaults. The catalog is code, so removing a key from the catalog
  makes any remaining database row unreachable; delete the row during the same
  release.
- The evaluator never writes DDL and the API never migrates. Run
  `cmd/migrate` before starting a release that introduces the feature tables.

## Tests

- `internal/platform/features`: catalog/parsing, every precedence layer,
  tenant scoping, TTL expiry, invalidation, stale serving, max staleness, and
  in-flight invalidation.
- `internal/modules/features/{service,handler}`: catalog merge, target tenant
  scoping, idempotent delete, validation, audit metadata, permissions, and
  route-level idempotency.
- `test/integration/feature_flags_test.go`: fresh-schema SQL, admin lifecycle,
  tenant/user precedence, cross-tenant rejection, audit metadata, and
  permission enforcement.
