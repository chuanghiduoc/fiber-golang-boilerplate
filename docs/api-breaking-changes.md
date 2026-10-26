# Intentional breaking API changes

CI's `API compatibility` job converts the base branch's and the
pull request's `docs/swagger.json` to OpenAPI 3 (`go run ./cmd/openapi3`) and
runs `oasdiff breaking --fail-on ERR`. A pull request that **edits this file**
acknowledges its breaking changes: the job still prints the oasdiff report but
does not fail. Editing the file in an earlier PR does not exempt later ones.

Add one entry per intentional break, newest first: the date, the PR, every
affected `METHOD /path`, what clients must change, and the migration path
(for example the `/api/v2` route that replaces it). Prefer a non-breaking
alternative (new optional field, new route, `/api/v2` via `app.V2Module`,
deprecation first) before adding an entry.

Reproduce the check locally:

```bash
go install github.com/oasdiff/oasdiff@v1.33.0
git show origin/main:docs/swagger.json > /tmp/base-swagger.json
go run ./cmd/openapi3 -in /tmp/base-swagger.json -out /tmp/base.json
go run ./cmd/openapi3 -in docs/swagger.json -out /tmp/pr.json
oasdiff breaking /tmp/base.json /tmp/pr.json --fail-on ERR
```

## Entries

None yet.
