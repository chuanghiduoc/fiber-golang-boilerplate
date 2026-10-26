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

### 2026-10-26 — MFA enrollment requires re-authentication

- `POST /api/v1/auth/mfa/totp/enroll` now requires a JSON body. Accounts with
  a local password must send `{"currentPassword": "<password>"}`; a
  Google-only account sends `{}` and must use a session created within the
  last 15 minutes. A missing body is `400`.
- `POST /api/v1/auth/mfa/totp/confirm` accepts the new optional
  `currentPassword` field. Password accounts must send it (a wrong password is
  the same `400` as the password-change flow); Google-only accounts omit it
  and need the recent session.
- Clients must re-authenticate before starting or confirming enrollment.
  There is no v2 route for this security fix; update clients in the same
  release. A pending enrollment is also valid for only 15 minutes
  (`409 auth.mfa_enrollment_expired`); start a new enrollment after that.
