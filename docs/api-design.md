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
Cursors are opaque base64url `(created_at:id)`. Handlers build `nextCursor`
with `pagination.NextCursor` from the last item's sort key. Offset helpers are
for small admin/export tables only.

Cursor SQL always uses the plain row comparison
`(created_at, id) < (@cursor_created_at, @cursor_id)`; repositories bind
`DBCursor.CreatedAtParam()`/`IDParam()`, which return `('infinity',
MaxInt64)` on the first page. Never write `NOT @has_cursor OR ...`: under a
generic (prepared) plan the OR form can only filter, so a deep page scans every
newer row (measured on 200k rows: 2,742 buffers vs 5 with the row comparison
as an index condition).

## Update semantics

Existing `PUT` routes keep their current, documented behavior:

- **Partial update (pointer DTO fields):** `PUT /users/me`, `PUT /users/:id`
  (`name`, `email`) and `PUT /admin/api-keys/:id` (`name`, `scopes`) change
  only the fields present; omitted fields are left unchanged.
- **Full replace:** `PUT /webhooks/endpoints/:id` and
  `PUT /webhooks/inbound-sources/:id` require the full representation
  (`url`/`status`, `name`/`slug`/`status`); omitted optional fields reset to
  their defaults. `PUT /admin/features/flags/{key}` likewise replaces the
  database default/kill switch/description, and the preference and target
  `PUT`s are upserts.
- **New modules:** use `PUT` only for full replacement and `PATCH` with
  pointer (or explicitly nullable) fields for partial updates.
- **RPC-style routes kept for compatibility:** `POST /files/upload` (multipart
  create) and the `POST /admin/api-keys/:id/revoke` alias of
  `DELETE /admin/api-keys/:id` stay as-is. New modules create with
  `POST /<collection>` and model state transitions as sub-resource actions
  only when no standard method fits.

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
- Every authenticated module route accepts both credential types through one
  chain (`app.Deps.Authenticate()`); the route's `authz.Require` decides
  access, so an API key reaches a route only when its scopes include the
  permission. The exception is `POST /auth/logout-all`, which is JWT-only.
- `POST /auth/logout-all` requires `sessions:revoke` (every built-in role holds
  it), revokes all refresh tokens, and bumps the token version, invalidating
  access tokens immediately.

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
- Authenticated requests use the JWT `tenant_id` claim or the API key's bound
  tenant. If an explicit `X-Tenant-Slug` resolves to a different tenant, the
  request fails with `403 permission.denied` and detail `tenant mismatch`.
  Zero or negative tenant claims are rejected with `401`.
- An API key without an explicit header remains usable for its own tenant; an
  explicit header is only accepted when it resolves to the key's tenant. The
  key owner and the owning tenant's status are re-read on every successful
  authentication, so bans, deletions, role changes, and tenant suspensions
  take effect immediately (`401`).
- Slug resolution for `X-Tenant-Slug`/`?tenant=` is cached in-process
  (`tenant.CachedResolver`: 30s for hits, 5s for unknown slugs, 1,024 entries
  per process). Suspending a tenant therefore reaches header-resolved public
  routes within 30s; authenticated JWT/API-key requests are rejected
  independently by the session/API-key tenant-status checks.
- One user belongs to one tenant; emails and Google IDs are unique per tenant.
  Login, registration, refresh, password reset, and email verification use the
  resolved request tenant, so non-default tenants must send `X-Tenant-Slug` on
  every call unless a documented browser-auth query fallback applies.
  Cross-tenant user/file/notification ids return `404`, refresh tokens fail as
  invalid (`401`), and reset/verification link tokens return `400`.
  Notification feed and preference reads are scoped to both the request tenant
  and the authenticated user.
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
- The scope includes the tenant ID and slug (plus user ID, or IP for public
  routes), so one key cannot replay a `201` across tenants.
- Auth token-minting endpoints are excluded by design.
- Secret-returning routes are excluded so plaintext secrets are never written
  to the shared response cache: API-key create/rotate and webhook
  endpoint/inbound-source create and `rotate-secret`. A retried rotation
  rotates again and returns the newest secret.

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
- Downloads are authenticated and tenant/ownership checked; there is no public
  `/uploads` tree. A cross-tenant id returns `404`, never `403`; the same owner
  check conceals another user's file within the tenant. Services enforce these
  boundaries with `authz.TenantPolicy` + `authz.OwnerPolicy`; admin services use
  the tenant policy only.
- Deletes are soft; `includeDeleted=true` exposes trash to the owner and
  `POST /files/:id/restore` requires `files:restore`.
- `Content-Disposition` follows RFC 6266/5987.

## Webhooks

- Admin/permission routes: `/api/v1/webhooks/endpoints`,
  `/api/v1/webhooks/deliveries`, and `/api/v1/webhooks/inbound-sources` with
  `webhooks:read` / `webhooks:write` (`admin` role).
- Endpoint create and inbound-source create return a plaintext `secret` once;
  reads never include it. Rotation returns the new secret once.
  Secret-returning creates and `rotate-secret` deliberately omit
  `Idempotency-Key` caching; update/delete/replay accept it.
- Every administration mutation (endpoint/source create, update, delete,
  rotate-secret, and delivery replay) is Tier 2: a required `audit.Guard`
  intent row is written first, so an unavailable audit store returns
  `503 system.audit_unavailable` without changing anything. The service's
  domain event (`webhook.endpoint.*`, `webhook.inbound_source.*`,
  `webhook.delivery.replayed`) is recorded as a child of that intent.
- Delivery list filters: `endpointId`, `status=pending|succeeded|dead`, plus
  cursor `limit`/`startingAfter`. `GET /deliveries/:id` includes the retained
  payload snapshot; list responses omit it.
- `POST /deliveries/:id/replay` creates a new delivery with the endpoint's
  current URL and active secret. The response is `201`.
- Outbound delivery is at-least-once. Requests carry `X-Webhook-Id`,
  `X-Webhook-Delivery` (dedupe key), `X-Webhook-Event`,
  `X-Webhook-Timestamp`, `X-Webhook-Signature: v1=<hex>`, and
  `X-Webhook-Attempt`. Sign `<timestamp>.<raw-body>` with HMAC-SHA256.
- Retryable responses are `408`, `425`, `429`, and `5xx`; all other `3xx`/`4xx`
  are permanent (`dead`). `Retry-After` is honored (seconds or HTTP date) and
  capped by config.
- `POST /api/v1/webhooks/inbound/{id}` is public and has no JWT, tenant header,
  or idempotency middleware. It requires `X-Webhook-Id`,
  `X-Webhook-Timestamp`, and `X-Webhook-Signature`; unknown/disabled sources
  return `404`, bad/stale signatures `401`, malformed JSON/IDs `400`. New
  events return `202`; a repeat `(source, X-Webhook-Id)` returns `200` with
  `duplicate: true`.
- Delivery targets are SSRF-checked at dial time; private/loopback/link-local
  targets require `WEBHOOK_SSRF_ALLOWLIST`. Redirects are never followed.
- See [Webhooks](webhooks.md) for the full signing, retry, secret, and
  operations contract.

## API keys

- `POST /admin/api-keys` returns the plaintext key once; list/get responses
  never contain it. Name, owner (`userId`, defaults to the caller), scopes, and
  optional `expiresAt` are recorded. Scopes must be known permissions and a
  subset of the owner's role.
- `PUT /admin/api-keys/:id` updates name/scopes, `DELETE /admin/api-keys/:id`
  or `POST /admin/api-keys/:id/revoke` revokes immediately (idempotent), and
  `POST /admin/api-keys/:id/rotate` issues a new secret while the old key stays
  valid for 24 hours unless revoked. Create, update, and rotate reject (`400`)
  any key scope the caller does not hold, so an API-key caller cannot mint or
  rotate a key broader than itself.
- All routes are tenant-scoped and return `404` for another tenant's key id.
  Lifecycle mutations accept `Idempotency-Key`; create and rotate deliberately
  omit it so a plaintext secret is never written to the shared response cache.
  Every lifecycle mutation (create/update/revoke/rotate) is Tier 1 audited: the
  service writes `api_key.created|updated|revoked|rotated` in the mutation
  transaction, so if the audit row cannot be written the change rolls back and
  the request fails with `503 system.audit_unavailable` (no secret is
  returned). Re-revoking an already revoked key writes no second row. Audit
  metadata never carries the secret or hash.

## Notifications

All notification routes require a JWT or a scoped API key and are scoped to the authenticated user
and request tenant. A notification id from another user or tenant returns
`404`, never `403`.

- `GET /notifications/` is cursor-paginated (`limit`, `startingAfter`) and
  accepts `unreadOnly=true`. The list uses the standard
  `{object,url,data,hasMore,nextCursor}` envelope.
- `GET /notifications/unread-count` returns `{count}` for the badge.
- `GET /notifications/:id` returns one owned notification.
- `POST /notifications/:id/read` sets `read_at` once and returns the row;
  repeating it is safe and does not move the timestamp.
- `POST /notifications/read-all` marks every unread row and returns
  `{updated}`.
- `POST /notifications/send` is the admin-only back-office send. It requires
  `notifications:send` and targets a user in the request tenant. It returns
  `201` with `notification` (null when the in-app channel is disabled) and
  `emailQueued`. It is Tier 2 audited: a required `audit.Guard` intent
  (`notification.sent`) is written first, so an unavailable audit store
  returns `503 system.audit_unavailable` and nothing is sent.

Preferences are per `(user, tenant, type, channel)` with channels `in_app` and
`email`:

- `GET /notification-preferences/` lists only explicit rows; a missing row
  means the channel is enabled.
- `PUT /notification-preferences/` upserts `{type, channel, enabled}`. `type`
  is a lower-case identifier (`^[a-z0-9][a-z0-9._-]*$`, max 100). The unique
  index makes the upsert idempotent.

Writes use `Idempotency-Key`. The caller's own state changes (mark read,
read-all, preference updates) are audited by the best-effort generic
mutating-request middleware. When the email channel is enabled, the
in-app row and the durable `email.send` job commit in one transaction; an
enqueue failure returns `500` and leaves no in-app row. A notification is
suppressed when both channels are disabled (create is a successful no-op; the
admin send response has `notification: null` and `emailQueued: false`). Email
delivery is at-least-once.

## Jobs and asynchronous work

- Email verification/reset, notification email, and other durable work are
  queued in PostgreSQL.
- Outbound webhook delivery uses the same queue (`webhook.delivery`) and is
  at-least-once; receivers deduplicate on `X-Webhook-Delivery`. Identity
  registration and file upload/delete emit `user.registered`,
  `file.uploaded`, and `file.deleted` through the contracts dispatcher; a
  dispatch failure is logged and never fails the committed mutation.
- Delivery is at-least-once; clients must tolerate duplicate emails. A failed
  notification enqueue rolls back the in-app row.
- Terminal jobs clear payloads; success/dead rows are retained 30 days.
- Email payloads are encrypted at rest; notification emails use the same
  `email.send`/`email.send.encrypted` kinds as identity emails.

## Feature flags

- Compile-time keys only; unknown keys return `404`. The evaluator precedence is
  force-off > kill switch > user target > tenant target > database default >
  environment > compiled default. See [Feature Flags](feature-flags.md).
- `GET /admin/features/flags` lists the catalog with the database default,
  kill switch, configured marker, and the caller's effective value/source.
- `GET /admin/features/flags/{key}` returns the flag and current-tenant targets.
- `PUT /admin/features/flags/{key}` replaces the database default, kill switch,
  and description. `killSwitch` is required; omitting `enabled` clears the
  database default (NULL).
- `DELETE /admin/features/flags/{key}` removes the database row, reverting to
  environment/compiled defaults; it is idempotent for a valid catalog key and
  returns `409` while any tenant still has a target for the flag.
- Flag-row writes (`PUT`/`DELETE /flags/{key}`) are process-wide and allowed
  only for admins of the platform tenant (id 1); other tenants get
  `403 permission.denied`.
- `GET`/`PUT /admin/features/flags/{key}/targets` list/upsert targets. Omit
  `userId` for a tenant-wide target; the tenant always comes from the request
  context. `DELETE .../targets/{id}` is scoped to the tenant and flag.
- Reads require `admin:features:read`; mutations require
  `admin:features:write`, accept `Idempotency-Key`, and are wrapped by a
  required `audit.Guard`. Targets are tenant-scoped. A user target from another
  tenant is rejected with `400`.
- A route gated by a disabled flag returns `404 feature.disabled`.

## Compatibility checklist

- New field: allowed.
- New enum value: allowed if clients are tolerant; document it.
- New required request field: breaking.
- Changed error code/status: breaking.
- New endpoint: additive.
- New permission: additive; grant it deliberately.
- New migration: fresh installs update the baseline; deployed DBs get a
  timestamped migration (`make migrate-create module=<owner> name=<purpose>`).
- Tenant-aware behavior: sending/omitting `X-Tenant-Slug` must preserve the
  default tenant when omitted; changing tenant selection is breaking.

## References

- Typed codes: platform-wide in `pkg/apperror/catalog.go`; module-owned in
  `internal/modules/<m>/catalog/errors.go`.
- Permissions: module-owned in `internal/modules/<m>/catalog/permissions.go`;
  shared back-office permissions in `internal/platform/authz/permissions.go`.
- Architecture: `docs/architecture.md`.
