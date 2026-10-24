# Contributing

## Commit Convention

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>
```

| Type | When |
|------|------|
| `feat` | New feature |
| `fix` | Bug fix |
| `refactor` | Code change that neither fixes a bug nor adds a feature |
| `docs` | Documentation only |
| `test` | Adding or updating tests |
| `chore` | Build, CI, deps, configs |
| `perf` | Performance improvement |

Scope is optional: `feat(auth): add email verification`, `fix(storage): handle S3 timeout`.

## Branch Workflow

```
main (protected)
  └── feat/add-notifications
  └── fix/refresh-token-race
  └── chore/upgrade-fiber
```

1. Create branch from `main`
2. Commit with conventional format
3. Push and open PR
4. CI must pass (Lint + Build + Migrations + Test + Integration + Security + Docker)
5. Squash merge into `main`

## Local Checks

```bash
make check          # go vet + golangci-lint + unit tests
go test -race ./... # required before merging
make test-integration   # Docker/Testcontainers, or set TEST_DATABASE_URL
go build ./cmd/migrate   # DDL entrypoint, also built by CI
```

Integration packages skip cleanly when `TEST_DATABASE_URL` is unset. Against a
local server, point it at a disposable database:

```bash
TEST_DATABASE_URL="postgres://${DB_USERNAME}:${DB_PASSWORD}@127.0.0.1:5432/apptest?sslmode=disable" \
  go test -tags=integration -race -count=1 ./...
```

CI's `Migrations` job applies the baseline up/down on a fresh PostgreSQL
service, then runs `cmd/migrate` twice. Reproduce it locally against a
disposable database (`make migrate-run`, then `make migrate-down`).

## Changelog

Update `CHANGELOG.md` in your PR. Format:

```markdown
## [Unreleased]

### Added
- Short description of new feature (#PR)

### Fixed
- Short description of bug fix (#PR)

### Changed
- Short description of change (#PR)

### Removed
- Short description of removal (#PR)
```

When releasing, move `[Unreleased]` entries under a version header:

```markdown
## [1.1.0] - 2026-02-23

### Added
- ...
```

## Database & Migrations

- Schema changes are a one-shot step: `make migrate-run` runs `cmd/migrate`,
  `make run` migrates before starting the API, `make seed` migrates before
  seeding, and Compose runs the `migrate` service before `app`.
- Only `cmd/migrate` applies DDL. `cmd/api` and `cmd/seed` assume the schema
  already exists, so replicas never race on migrations. The migrate command
  uses `config.LoadDatabase` and needs only `DB_*` environment variables; it
  rejects `DB_SSLMODE=disable` in production and verifies required tables after
  migrating.
- Fresh installs start from one baseline schema file:
  `migrations/000001_init_schema.up.sql` (and its `.down.sql`).
- Once a database is deployed, never edit a shipped migration. Add the next
  numbered migration instead (`make migrate-create name=...`).
- One module owns each table. Cross-module access goes through a contract or
  read model, not another module's repository.
- Every tenant-owned table carries
  `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)`, and its indexes
  lead with `tenant_id`. The `default` tenant (id 1) preserves compatibility for
  header-less local/test requests; production requires `X-Tenant-Slug`
  (`TENANT_REQUIRE_HEADER=true`), and emails/Google IDs are unique per tenant.
- The `users` last-admin trigger is per tenant. Application checks must use the
  same tenant scope as the trigger.
- Identity and files are tenant-scoped: their queries filter `tenant_id`,
  repositories derive it from `tenant.EffectiveID(ctx)` (tenant-1 fallback is
  for jobs/internal/tests only, never a substitute for HTTP tenant resolution),
  public auth flows resolve the tenant, and Google state carries it. Reset and
  verification links carry `?tenant=<slug>`; browser-facing auth paths accept
  that validated query fallback. Uploads use `<tenantID>/<userID>/<uuid>.<ext>`.
  Idempotency keys include the tenant; logout revokes refresh tokens by
  possession. RLS and per-tenant limits remain future work, so do not describe
  the whole application as fully isolated.

### Upgrading an existing database

The consolidated `000001` is a fresh-install baseline (old `000001`–`000003`
plus `jobs`/`audit_events`). Do not run it against a database that already
applied the old migrations, and do not reset `schema_migrations` to force it.
Keep the applied history, restore the old `000002`/`000003` files from git
history (golang-migrate requires the recorded version to exist in the source),
and add a migration with only the missing objects, numbered above the
database's current version (a fully migrated old deployment is at 3, so
`000004`+). `make migrate-create` derives the number from this repository, so
set or rename it explicitly. A fresh schema created out-of-band that already
matches `000001` can be baselined manually after verification (for example, the
golang-migrate CLI's `force 1`) instead of re-running the file.

The multi-tenant foundation also changes `000001` (tenants table, `tenant_id`
columns, per-tenant indexes and last-admin trigger). Existing databases need a
new numbered migration or reviewed manual DDL; see the README's "Upgrading an
existing database" section. Do not assume the baseline created the tenant
objects on an already-deployed database. Identity/files isolation needs no
further schema, but legacy access tokens without `tenant_id` are rejected after
the upgrade (users must log in again); old token rows remain on tenant 1.
Existing files keep their old `<user>/<uuid>` storage keys; only new uploads use
the `<tenantID>/<userID>/<uuid>` layout. If you copy blobs, update
`files.storage_path` atomically.

## Adding a New Module

Run `scripts/new-module.sh <name>` and follow
[`docs/module-template.md`](docs/module-template.md). The runtime behavior of
the resulting module is covered by [`docs/operations.md`](docs/operations.md).

1. Create `internal/modules/<name>/module.go` implementing `app.Module`
   (`Name`, `RegisterRoutes`). Keep the module's handler/service/repository/DTO
   inside that directory.
2. Add permissions to `internal/platform/authz` and protect every route with
   `authz.Require(...)`. The legacy exceptions are documented in the
   [module template](docs/module-template.md#legacy-exceptions-do-not-copy);
   do not copy them.
3. Add queries in `internal/modules/<name>/queries/<name>.sql`, add the module
   block to `sqlc.yaml`, and run `make sqlc-generate`.
4. For a deployed DB, add a numbered migration; for this boilerplate's fresh
   installs, update the single init schema.
5. Register the module once in `cmd/api/main.go`.
6. Add unit tests plus an integration test; `test/arch` rejects cross-module
   imports.
7. Run `make swagger`, `make check`, and `go test -race ./...`.
8. Update `CHANGELOG.md`.

> `identity`, `files`, and `admin` own all of their DTOs, repositories,
> services, and handlers under `internal/modules/<name>`. `admin` consumes
> identity/files only through `internal/contracts`; it does not import their
> internals. Each module with SQL owns its generated `sqlc/` package (`admin`
> is contract-only); never import another module's sqlc types, and no legacy
> handler/service/DTO/repository packages remain.

## Code Style

- Error handling: return `*apperror.AppError` via the typed catalog
  (`apperror.NewCode(...)`), not raw errors or magic strings
- Responses: use `pkg/response` helpers, not `c.JSON()`
- Roles: use the owning module's constants (`identity/dto` or `admin/dto`), not
  magic strings
- Permissions: use `authz.Permission*`, not ad-hoc role checks
- Int conversion: use `pagination.clampInt32()`, not direct `int32()` cast
- Durable background work: enqueue in `internal/platform/jobs`; never use
  `async.Go` for work that must survive a restart
- Tests: stdlib `testing`; `testify` is allowed where it improves assertions
