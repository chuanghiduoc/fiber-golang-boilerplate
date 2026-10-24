# API Design System

Conventions for every HTTP endpoint in this service. Changes here are public
contract changes; treat them as breaking unless additive.

## Base and versioning

- Base path: `/api/v1`. `/api/v2` exists only when at least one module
  implements `app.V2Module`; Swagger documents v1.
- The path selects the surface. `Accept-Version` is an optional assertion
  (`1`, `v1`, `2`, or `v2`); when present it must match the path. A mismatch
  returns `406 api.version_unsupported`.
- Version-shaped paths that are not mounted (for example `/api/v3`) also return
  `406 api.version_unsupported`, never a generic 404.
- Every versioned response carries `API-Version` (the serving version) and
  `Vary: Accept-Version`. Deprecated versions add `Deprecation` (RFC 9745
  `@<unix-seconds>`), `Sunset` (HTTP-date), and `Link` (`rel="deprecation"` to
  `DocsURL`, `rel="successor-version"` to the successor path).
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
- `title` and `errors[].message` are localized from `Accept-Language` (`en` /
  `vi`) when i18n is enabled. `detail` is localized only when the handler
  attaches `AppError.DetailKey`; otherwise the supplied text is preserved.
  Localization never changes `code`, `type`, `status`, `instance`, or paths.
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
| 406 | Accept-Version mismatch or unsupported `/api/vN` |
| 409 | Conflict, duplicate, last-admin, idempotency body mismatch |
| 413 | Upload too large |
| 415 | Unsupported media type |
| 422 | Validation failed |
| 429 | Rate limited |
| 500 | Internal error |
| 503 | Dependency degraded / not ready, or required audit intent unavailable |
| 504 | Request timeout |

## Authentication and authorization

- `Authorization: Bearer <jwt>` for protected routes.
- JWT pins HS256, requires `exp`/`iat`/`aud`/`iss`, and carries `token_version`
  plus `tenant_id`. Tokens minted before multi-tenancy omit `tenant_id` and are
  rejected with 401; clients must log in again after the upgrade.
- Access tokens are short-lived; refresh tokens rotate atomically, are
  single-use/version-bound, and are scoped to the tenant that issued them.
- Permissions live in `internal/platform/authz`; route middleware uses
  `authz.Require`. Never check role strings ad hoc in handlers.
- `POST /auth/logout-all` revokes all refresh tokens and bumps the token
  version, invalidating access tokens immediately.

## Tenants

- `X-Tenant-Slug` (lowercase `[a-z0-9-]{1,63}`) selects the request tenant on
  `/api/v1` routes. In local/test compatibility mode, omitting it resolves to
  `TENANT_DEFAULT_SLUG` (`default`). `TENANT_REQUIRE_HEADER=true` rejects
  header-less requests instead and is **required in production** so header-less
  HTTP writes cannot fall back to tenant 1.
- Invalid, unknown, or suspended slugs (including the configured default)
  return `404` with detail `tenant not found`. Startup existence checks ignore
  status: a suspended configured/default tenant does not crash the API or seed
  commands; its requests return `404` until reactivated.
- Validated `?tenant=<slug>` is accepted as a fallback on `GET /auth/google`
  and the reset/verify endpoints because browser navigation and emailed links
  cannot set a custom header. An explicit `X-Tenant-Slug` takes precedence.
- Authenticated requests use the JWT `tenant_id` claim. If an explicit
  `X-Tenant-Slug` resolves to a different tenant, the request fails with
  `403 permission.denied` and detail `tenant mismatch`. Zero or negative tenant
  claims are rejected with `401`.
- One user belongs to one tenant; emails and Google IDs are unique per tenant.
  Login, registration, refresh, password reset, and email verification use the
  resolved request tenant, so non-default tenants must send `X-Tenant-Slug` on
  every call unless a documented browser-auth query fallback applies.
  Cross-tenant user/file ids return `404`, refresh tokens fail as invalid
  (`401`), and reset/verification link tokens return `400`.
- Password-reset and email-verification links include `?tenant=<slug>` in
  addition to the opaque token. Token rows are tenant-bound, so completing a
  link under another tenant fails without consuming the token.
- Google OAuth binds the tenant into the signed `state`; the callback ignores
  `X-Tenant-Slug`, uses that tenant, and rejects unknown or suspended tenants
  (including one suspended mid-flow). Header-required mode does not block the
  callback because tenant middleware exempts it and the handler validates
  `state`.
- `POST /auth/logout` revokes the presented refresh token by possession, with or
  without `X-Tenant-Slug`; it returns `204` even if the token was already gone.
- Idempotency scope includes method, path, tenant ID/slug, and caller (user ID
  or public-route IP). The same key/body/IP in another tenant creates a new
  response rather than replaying a cross-tenant `201`.
- `UserResponse`/`FileResponse` include additive `tenantId` fields. Tokens
  without a `tenant_id` claim are rejected (fail-closed).
- **Remaining:** row-level security and per-tenant limits/quotas.

## Headers

- `X-Request-Id` is accepted if valid, otherwise generated; echoed on responses.
- `X-Tenant-Slug` selects the tenant; it is in the default CORS allow list.
  Browser-facing auth paths also accept a validated `?tenant=<slug>` fallback
  (header wins).
- `Idempotency-Key` is opt-in on mutating routes; 1-128 chars of
  `[A-Za-z0-9._:-]`. Replays return `Idempotent-Replayed: true`.
- `Accept-Language` negotiates `en`/`vi` with q-values, case-insensitive tags,
  region truncation (`vi-VN` → `vi`), wildcards, and `I18N_DEFAULT_LOCALE`
  fallback. Responses set `Content-Language` and `Vary: Accept-Language`.
- `Accept-Version` asserts the path version; responses set `API-Version` and
  `Vary: Accept-Version`, plus deprecation headers when applicable.
- `Location`, `ETag`, locale, and version metadata are exposed through CORS.
- Rate limits are tiered (strict/normal/relaxed) and keyed per user when
  authenticated, otherwise per IP. `429` is retryable.

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
- The scope includes the tenant ID and slug (plus user ID, or IP for public
  routes), so one key cannot replay a `201` across tenants.
- Auth token-minting endpoints are excluded by design.

## Audit and fail-closed behavior

- Privileged mutations are audited before or atomically with the write. A
  Tier 1 mutation whose audit row cannot be committed rolls back and returns
  `503` with code `system.audit_unavailable` (retryable). A Tier 2 route whose
  required pre-write intent cannot be stored returns the same 503 before the
  handler runs; the mutation does not execute.
- Retrying after 503 is safe when using the same `Idempotency-Key` where the
  route supports it. Replayed responses are not re-audited.
- Audit metadata is sanitized: passwords, tokens, emails, and URLs are
  redacted, and size/depth are bounded. Do not design clients that depend on
  request values being echoed back through audit endpoints (none are public).
- A completion failure after a Tier 2 mutation cannot undo the write; operators
  should alert on `audit_completion_failures_total` and stale `started` rows.

## Files

- `POST /files/upload` accepts multipart `file`, enforces size/MIME allowlist,
  and stores blobs under `<tenantID>/<userID>/<uuid>.<ext>`.
- Downloads are authenticated and tenant/ownership-checked; there is no public
  `/uploads` tree. A cross-tenant id returns `404`, never `403`.
- Deletes are soft; `includeDeleted=true` exposes trash to the owner.
- `Content-Disposition` follows RFC 6266/5987.

## Jobs and asynchronous work

- Email verification/reset and other durable work are queued in PostgreSQL.
- Delivery is at-least-once; clients must tolerate duplicate emails.
- Terminal jobs clear payloads; success/dead rows are retained 30 days.
- Email payloads are encrypted at rest.

## Feature flags

- Compile-time keys only; unknown keys return `404`. The evaluator precedence is
  force-off > kill switch > user target > tenant target > database default >
  environment > compiled default. See [Feature Flags](feature-flags.md).
- `GET /admin/features/flags` lists the catalog with the database default,
  kill switch, configured marker, and the caller's effective value/source.
- `GET /admin/features/flags/{key}` returns the flag and current-tenant targets.
- `PUT /admin/features/flags/{key}` replaces the database default, kill switch,
  and description. `enabled` and `killSwitch` are required.
- `DELETE /admin/features/flags/{key}` removes the database row and cascades
  targets, reverting to environment/compiled defaults; it is idempotent for a
  valid catalog key.
- `GET`/`PUT /admin/features/flags/{key}/targets` list/upsert targets. Omit
  `userId` for a tenant-wide target; the tenant always comes from the request
  context. `DELETE .../targets/{id}` is scoped to the tenant and flag.
- Reads require `admin:features:read`; mutations require
  `admin:features:write` and accept `Idempotency-Key`. Flag definitions are
  process-wide; targets are tenant-scoped. A user target from another tenant is
  rejected with `400`.

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
