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
Integration, Security, Docker, and (on PRs) API compatibility. [`.github/CODEOWNERS`](.github/CODEOWNERS)
assigns reviewers per platform area and per module (code, contracts, and
migrations); add your module's paths there when you create one.

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
make test-integration    # testcontainers, or set TEST_DATABASE_URL
```

Against a local server, point `TEST_DATABASE_URL` at a disposable database
and apply the schema first (`make migrate-run`). Run the integration suite
with parallel packages as CI does; serial runs hide cross-package races:

```bash
TEST_DATABASE_URL="postgres://${DB_USERNAME}:${DB_PASSWORD}@127.0.0.1:5432/apptest?sslmode=disable" \
  go test -tags=integration -race -count=1 ./...
```

Migrations and adding a module are covered in
[docs/operations.md](docs/operations.md#7-migrations) and
[docs/module-template.md](docs/module-template.md).

## Changelog

Add a line under `## [Unreleased]` in `CHANGELOG.md` (`Added`, `Changed`,
`Fixed`, `Removed`, or `Breaking`) with the PR number. When releasing, move
the entries under a version header.

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
