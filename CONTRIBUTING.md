# Contributing

## Workflow

```mermaid
flowchart LR
    A["Branch from main<br/>feat/..., fix/..., chore/..."] --> B["Commit with<br/>Conventional Commits"]
    B --> C["Local checks"]
    C --> D["Push + open PR<br/>(update CHANGELOG.md)"]
    D --> E{"CI green?"}
    E -->|no| B
    E -->|yes| F["Squash merge<br/>into main"]
```

`main` is protected. CI must pass: Lint, Build, Manifests, Migrations, Test,
Integration, Security, and Docker. [`.github/CODEOWNERS`](.github/CODEOWNERS)
assigns reviewers per platform area and per module (code, contracts, and
migrations); add your module's paths there when you create one.

## Architecture decisions

Decisions that change a boundary (layering, tenancy, authorization,
migrations, API or event contracts) are recorded as ADRs in
[`docs/adr/`](docs/adr/README.md). Copy
[`docs/adr/template.md`](docs/adr/template.md), take the next number, and
link the ADR from the PR. Accepted ADRs are not rewritten; supersede them with
a new one.

## Commit Convention

Follow [Conventional Commits](https://www.conventionalcommits.org/):
`<type>(<scope>): <description>`. Scope is optional, e.g.
`feat(auth): add email verification`, `fix(storage): handle S3 timeout`.

| Type | When |
|------|------|
| `feat` | New feature |
| `fix` | Bug fix |
| `refactor` | Code change that neither fixes a bug nor adds a feature |
| `docs` | Documentation only |
| `test` | Adding or updating tests |
| `chore` | Build, CI, deps, configs |
| `perf` | Performance improvement |

## Local Checks

```bash
make check               # go vet + golangci-lint + arch tests + unit tests
make test-race           # go test -race; required before merging
make test-integration    # Docker/Testcontainers, or set TEST_DATABASE_URL
go build ./cmd/migrate   # DDL entrypoint, also built by CI
```

Integration packages skip cleanly when `TEST_DATABASE_URL` is unset. Against a
local server, point it at a disposable database and apply the schema first
(`make migrate-run` with matching `DB_*` settings); some platform integration
tests use that database as-is:

```bash
TEST_DATABASE_URL="postgres://${DB_USERNAME}:${DB_PASSWORD}@127.0.0.1:5432/apptest?sslmode=disable" \
  go test -tags=integration -race -count=1 ./...
```

CI's `Migrations` job applies the baseline up/down on a fresh PostgreSQL
service, then runs `cmd/migrate` twice. Reproduce it against a disposable
database (`make migrate-run`, then `make migrate-down CONFIRM=yes`, which
drops the whole schema).

## Changelog

Update `CHANGELOG.md` in your PR:

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

When releasing, move `[Unreleased]` entries under a version header such as
`## [1.1.0] - 2026-02-23`.

## Database & Migrations

| Rule | Detail |
|------|--------|
| One DDL entrypoint | Only `cmd/migrate` applies DDL. `make migrate-run` runs it, `make run`/`make seed` migrate first, Compose runs `migrate` before `app`. `cmd/api`/`cmd/seed` assume the schema exists, so replicas never race. |
| Migrate config | `config.LoadDatabase` needs only `DB_*`; `DB_SSLMODE=disable\|allow\|prefer` is rejected in production. It verifies required tables, indexes, and triggers after migrating. |
| Fresh installs | One baseline: `migrations/000001_init_schema.up.sql` (and `.down.sql`). |
| Deployed databases | Never edit a shipped migration. Add one with `make migrate-create module=<owner> name=<purpose>` → `<UTC YYYYMMDDHHMMSS>_<owner>_<purpose>.{up,down}.sql`; `test/arch` rejects duplicate versions, missing up/down pairs, and non-timestamp versions. |
| Ownership | One module owns each table; cross-module access goes through a contract or read model, never another module's repository. |
| Tenancy | Every tenant-owned table has `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)` and `tenant_id`-leading indexes. Tenant 1 (`default`) serves header-less local/test requests; production requires `X-Tenant-Slug` (`TENANT_REQUIRE_HEADER=true`). Emails and Google IDs are unique per tenant. |
| Last admin | The `users` last-admin trigger is per tenant; application checks must use the same scope. |
| Scoped queries | Repositories derive the tenant from `tenant.EffectiveID(ctx)` (tenant-1 fallback is for jobs/internal/tests only, never a substitute for HTTP resolution). Public auth flows resolve the tenant, Google `state` carries it, reset/verify links carry `?tenant=<slug>`, uploads use `<tenantID>/<userID>/<uuid>.<ext>`, idempotency keys include the tenant, and logout revokes by possession. RLS and per-tenant limits are future work; do not describe the application as fully isolated. |

### Upgrading an existing database

The consolidated `000001` is a fresh-install baseline (old `000001`–`000003`
plus every later table). Never run it against a database that applied the old
migrations (normally at version 3), and never reset `schema_migrations` to
force it. Do **not** restore the old `000002`/`000003` files:
`test/arch/migrations_test.go` rejects sequential versions other than `1`, and
a fresh install would run them after the new baseline.

The supported upgrade is a one-time, reviewed manual step (full DDL checklist in
the README's [Upgrading an existing database](README.md#upgrading-an-existing-database)):

1. Take a verified backup/PITR snapshot.
2. Apply the missing DDL with `psql` in one transaction so the schema matches
   `000001_init_schema.up.sql` (tenants and default tenant, `tenant_id`
   columns, tenant-leading indexes, per-tenant last-admin trigger, feature-flag,
   notification, and webhook tables, every `ON CONFLICT` unique index, audit
   triggers).
3. Only after the schema matches, record the baseline:
   `migrate -path migrations -database "$DSN" force 1`. This rewrites the
   version, not the schema; never use it to skip DDL that was not applied.
4. Run `cmd/migrate`. It applies newer timestamped migrations and verifies the
   schema, listing anything step 2 missed.

A fresh schema created out-of-band that already matches `000001` can be
baselined the same way (`force 1` after verification) instead of re-running
the file.

After the upgrade, legacy access tokens without `tenant_id` are rejected (users
log in again) and old token rows stay on tenant 1. Existing files keep their
`<user>/<uuid>` storage keys; only new uploads use `<tenantID>/<userID>/<uuid>`.
If you copy blobs, update `files.storage_path` atomically.

## Adding a New Module

Run `make gen module=<name>` (the Go generator `cmd/gen`; `scripts/new-module.sh
<name>` is a wrapper for it) and follow
[`docs/module-template.md`](docs/module-template.md); runtime behavior is
covered by [`docs/operations.md`](docs/operations.md).

1. The generator creates `module.go`, `build.go` (`Build(infra, Settings)
   (*Module, Provides, error)`), `catalog/` (permissions, error codes, en/vi
   strings), the ADR 0002 layers with `service/ports.go`, `queries/`, the
   module's own `sqlc.yaml`, tests, a `<UTC ts>_<name>_init` migration pair,
   and the `internal/bootstrap/modules.go` entry, then builds, vets, and tests
   (rolling everything back on failure). Keep everything in that directory.
2. Add slices with `make gen resource=<r> module=<name>` (tenant-scoped CRUD
   with migration, queries, routes, permissions, and unit + integration tests)
   and `make gen command=<Name> module=<name>` / `query=<Name>`. Add
   `dry_run=1` to preview a diff.
3. Adjust `catalog/permissions.go` (generated permissions are admin-only) and
   protect every route with `authzhttp.Require(...)` (`test/arch` enforces
   it). Do not copy the
   [legacy exceptions](docs/module-template.md#legacy-exceptions-do-not-copy).
4. Other schema changes: `make migrate-create module=<name> name=<purpose>`,
   then `make sqlc-generate` (runs every `internal/modules/*/sqlc.yaml`).
5. Pass module-owned configuration as the module's `Settings`, filled in
   `internal/bootstrap/modules.go`.
6. Add unit tests plus an integration test; `test/arch` rejects cross-module
   imports, Fiber in `service`/`repository` packages, and new
   [ADR 0002](docs/adr/0002-module-layering.md) layering violations (never add
   lines to `test/arch/layering_allowlist.txt`; delete the ones you fix).
7. Run `make swagger`, `make check`, and `go test -race ./...`.
8. Update `CHANGELOG.md`.

Each module with SQL owns its DTOs, repositories, services, handlers, and
generated `sqlc/` package (`admin` is contract-only and reaches identity/files
through `internal/contracts`). Never import another module's internals or sqlc
types.

## Code Style

| Concern | Do | Don't |
|---------|----|-------|
| Errors | Return `*apperror.AppError` via the typed catalog (`apperror.NewCode(...)`) | Raw errors or magic strings |
| Responses | `pkg/response` helpers | `c.JSON()` |
| Roles | The owning module's constants (`identity/dto` or `admin/dto`) | Magic strings |
| Permissions | `authzhttp.Require` with catalog permissions on routes; `authz.Authorize` in services | Ad-hoc role checks; `authz.SystemContext` derived from a request |
| Layering | Services return service types; handlers map them to DTOs; Fiber only in `handler/` and `*http` adapters | sqlc, pgx, Fiber, or `dto/` imports in `service/` |
| Int conversion | `pkg/pagination` helpers (they clamp to `int32`) | Direct `int32()` casts |
| Durable work | Enqueue in `internal/platform/jobs` | `async.Go` for work that must survive a restart |
| Tests | Stdlib `testing`; `testify` where it improves assertions | Docker in unit tests |
