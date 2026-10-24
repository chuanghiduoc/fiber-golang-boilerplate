# API Design System

Conventions for every HTTP endpoint in this service. Changes here are public
contract changes; treat them as breaking unless additive.

## Base and versioning

- Base path: `/api/v1`.
- Version in the path, never in a header.
- Additive fields are non-breaking. Removing/renaming fields, changing types,
  status codes, or error codes requires a new major version or a documented
  deprecation window.

## JSON conventions

- camelCase keys (`accessToken`, `nextCursor`).
- IDs are JSON numbers (int64). Timestamps are RFC 3339 UTC strings.
- Optional/absent values are omitted or `null`; empty lists are `[]`, never
  `null`.
- Requests reject unknown fields and trailing JSON values.
- Request bodies use `application/json`; uploads use `multipart/form-data`.

## Success responses

- `200 OK` returns the resource directly (no `{data,meta}` wrapper).
- `201 Created` returns the created resource; `Location` where applicable.
- `204 No Content` has no body.
- Lists use a Stripe-style envelope:

```json
{
  "object": "list",
  "url": "/api/v1/users",
  "data": [],
  "hasMore": false,
  "nextCursor": null
}
```

Pagination is forward-only: `limit` (1-100, default 20) + `startingAfter`.
Cursors are opaque base64url `(created_at:id)`. Offset helpers are for small
admin/export tables only.

## Errors (RFC 9457)

All errors use `application/problem+json`:

```json
{
  "type": "/errors/user-email-taken",
  "title": "Email already in use",
  "status": 409,
  "code": "user.email_taken",
  "detail": "email already registered",
  "instance": "/api/v1/auth/register",
  "requestId": "...",
  "timestamp": "2026-10-24T12:00:00.000Z",
  "errors": [{"path": "email", "code": "invalid_email", "message": "..."}]
}
```

Rules:

- `code` is the stable, dotted catalog identifier; clients may branch on it.
- `detail` is public text and never contains SQL, stack traces, or internals.
- `type` is `ERROR_DOCS_BASE_URL` + the catalog `DocPath`.
- Validation failures use `422` with a flat `errors[]`.
- Authorization failures use `403 permission.denied`.
- Do not expose whether another user's resource exists; return `404`.

## Status codes

| Code | Meaning |
|---|---|
| 200 | Read/update success |
| 201 | Resource created |
| 204 | Deleted/no content |
| 400 | Malformed request / invalid token |
| 401 | Missing/invalid authentication |
| 403 | Authenticated but not permitted |
| 404 | Not found (including non-owned resources) |
| 409 | Conflict, duplicate, last-admin, idempotency body mismatch |
| 413 | Upload too large |
| 415 | Unsupported media type |
| 422 | Validation failed |
| 429 | Rate limited |
| 500 | Internal error |
| 503 | Dependency degraded / not ready |
| 504 | Request timeout |

## Authentication and authorization

- `Authorization: Bearer <jwt>` for protected routes.
- `Authorization: ApiKey <prefix>_<secret>` for API-key clients. The secret is
  32 random bytes encoded as unpadded base64url; only its SHA-256 hash is
  stored. Keys are bound to one tenant and user, carry an explicit
  `authz.Permission` scope list, and may expire or be revoked. Rotating a key
  issues a replacement and leaves the previous key valid for a 24-hour grace
  window unless it is revoked; revocation is immediate. A key is shown once at
  creation/rotation. Admin lifecycle routes live under `/admin/api-keys` and
  require `admin:api_keys:read|create|update|revoke|rotate`. Successful API-key
  authentication sets `user_id`, `tenant_id`, `api_key_id`, and
  `auth_method=api_key`; JWT requests set `auth_method=jwt`.
- JWT pins HS256, requires `exp`/`iat`/`aud`/`iss`, and carries `token_version`
  plus `tenant_id`. Tokens minted before multi-tenancy omit `tenant_id` and are
  rejected with 401; clients must log in again after the upgrade.
- Access tokens are short-lived; refresh tokens rotate atomically, are
  single-use/version-bound, and are scoped to the tenant that issued them.
- Permissions live in `internal/platform/authz`; route middleware uses
  `authz.Require`. Never check role strings ad hoc in handlers. API-key scopes
  are intersected with the owner's current role, so a scoped key can neither
  exceed its recorded scopes nor retain capabilities after a demotion.
- Attribute-based checks use `authz.Subject` (explicit permissions, tenant,
  owner, cross-tenant flag, attributes), `Policy`/`PolicySet`, `TenantPolicy`,
  and `OwnerPolicy`. `Authorize` performs an RBAC pre-check followed by
  deny-overrides policy evaluation: deny maps to `403 permission.denied` and
  conceal maps to `404` so cross-tenant/foreign-owner resources are not
  disclosed.
- `POST /auth/logout-all` revokes all refresh tokens and bumps the token
  version, invalidating access tokens immediately.

## Tenants

- `X-Tenant-Slug` (lowercase `[a-z0-9-]{1,63}`) selects the request tenant on
  `/api/v1` routes. Omitting it resolves to `TENANT_DEFAULT_SLUG` (`default`),
  preserving pre-multi-tenant behavior.
- Invalid, unknown, or suspended slugs return `404` with detail
  `tenant not found`. `TENANT_REQUIRE_HEADER=true` rejects header-less requests
  the same way.
- Authenticated requests use the JWT `tenant_id` claim or the API key's bound
  tenant. If an explicit `X-Tenant-Slug` resolves to a different tenant, the
  request fails with `403 permission.denied` and detail `tenant mismatch`.
- An API key without an explicit header remains usable for its own tenant; an
  explicit header is only accepted when it resolves to the key's tenant. The
  key owner is re-read on every successful authentication, so bans, deletions,
  and role changes take effect immediately.
- One user belongs to one tenant; emails and Google IDs are unique per tenant.
  Login, registration, refresh, password reset, and email verification use the
  resolved request tenant, so non-default tenants must send `X-Tenant-Slug` on
  every call. Cross-tenant user/file ids return 404 and cross-tenant tokens fail
  as invalid.
- Google OAuth binds the tenant into `state`; the callback uses that tenant
  rather than the header and rejects a tenant suspended mid-flow.
- `UserResponse`/`FileResponse` include additive `tenantId` fields. Tokens
  without a `tenant_id` claim are rejected (fail-closed).
- **Remaining:** row-level security and per-tenant limits/quotas.

## Headers

- `X-Request-Id` is accepted if valid, otherwise generated; echoed on responses.
- `X-Tenant-Slug` selects the tenant; it is in the default CORS allow list.
- `Idempotency-Key` is opt-in on mutating routes; 1-128 chars of
  `[A-Za-z0-9._:-]`. Replays return `Idempotent-Replayed: true`.
- `Location`, `ETag` are returned where applicable and exposed through CORS.
- Rate limits are tiered (strict/normal/relaxed) and keyed per API key when
  authenticated with `ApiKey`, otherwise per user, then per IP. `429` is
  retryable. Rotating a key moves subsequent requests to a new bucket.

## Rate limits and quotas

- Configured via `RATE_LIMIT_*`; separate keyspaces per tier.
- Cache-backed (Redis in production); memory is single-replica only.
- Login attempts, password-reset requests, and verification resends have
  dedicated lockouts in addition to route limits.

## Idempotency

- Send `Idempotency-Key` on POST/PUT/PATCH/DELETE when retries are possible.
- First 2xx response is cached for 24h and replayed.
- Concurrent duplicates return `409` while the first is in progress.
- Non-multipart bodies up to 1 MiB are SHA-256-bound; same key with a different
  body returns `409`.
- Auth token-minting endpoints are excluded by design.

## Files

- `POST /files/upload` accepts multipart `file`, enforces size/MIME allowlist,
  and stores blobs under `<tenantID>/<userID>/<uuid>.<ext>`.
- Downloads are authenticated and tenant/ownership checked; there is no public
  `/uploads` tree. A cross-tenant id returns `404`, never `403`; the same owner
  check conceals another user's file within the tenant. Services enforce these
  boundaries with `authz.TenantPolicy` + `authz.OwnerPolicy`; admin services use
  the tenant policy only.
- Deletes are soft; `includeDeleted=true` exposes trash to the owner and
  `POST /files/:id/restore` requires `files:restore`.
- `Content-Disposition` follows RFC 6266/5987.

## API keys

- `POST /admin/api-keys` returns the plaintext key once; list/get responses
  never contain it. Name, owner (`userId`, defaults to the caller), scopes, and
  optional `expiresAt` are recorded. Scopes must be known permissions and a
  subset of the owner's role.
- `PUT /admin/api-keys/:id` updates name/scopes, `DELETE /admin/api-keys/:id`
  or `POST /admin/api-keys/:id/revoke` revokes immediately (idempotent), and
  `POST /admin/api-keys/:id/rotate` issues a new secret while the old key stays
  valid for 24 hours unless revoked.
- All routes are tenant-scoped, return `404` for another tenant's key id,
  accept `Idempotency-Key`, and attach `apiKeyId`/scope/rotation metadata to the
  audit event (never the secret or hash).

## Jobs and asynchronous work

- Email verification/reset and other durable work are queued in PostgreSQL.
- Delivery is at-least-once; clients must tolerate duplicate emails.
- Terminal jobs clear payloads; success/dead rows are retained 30 days.
- Email payloads are encrypted at rest.

## Compatibility checklist

- New field: allowed.
- New enum value: allowed if clients are tolerant; document it.
- New required request field: breaking.
- Changed error code/status: breaking.
- New endpoint: additive.
- New permission: additive; grant it deliberately.
- New migration: fresh installs update the baseline; deployed DBs get a
  numbered migration.
- Tenant-aware behavior: sending/omitting `X-Tenant-Slug` must preserve the
  default tenant when omitted; changing tenant selection is breaking.

## References

- Typed codes: `pkg/apperror/catalog.go`.
- Permissions: `internal/platform/authz/permissions.go`.
- Architecture: `docs/architecture.md`.
