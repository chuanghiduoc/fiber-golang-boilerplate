# API Design System

Conventions for every HTTP endpoint in this service. The complete target route
table is [api-routes.md](api-routes.md); the generated OpenAPI 3.1 document is
[`openapi.yaml`](openapi.yaml). Changes here are public contract changes; treat
them as breaking unless additive. Middleware order and module boundaries are in
[Architecture](architecture.md#request-lifecycle).

## Base and versioning

- **One surface:** `/api/v1`. There is no `/api/v2`, no `Accept-Version`
  assertion, and no `API-Version`/`Deprecation`/`Sunset` header machinery. The
  path is the version.
- A version-shaped path that is not mounted (for example `/api/v3`) is an
  ordinary `404 resource.not_found`; the client asked for a resource that does
  not exist.
- Additive fields are non-breaking. Removing/renaming fields or changing types,
  status codes, or error codes requires a documented break in a release that
  can coordinate clients; there is no deployed compatibility window yet.
- `cmd/openapi` writes the OpenAPI 3.1 document without a database, and CI
  compares base vs pull-request specs with `oasdiff breaking`.

## Registration and policy

Every operation is registered through **one** helper,
`internal/platform/httpapi.Register`:

```go
httpapi.Register(api, httpapi.Op{
    Method: http.MethodGet, Path: "/api/v1/files",
    OperationID: "listFiles", Tags: []string{"files"}, Summary: "List files",
}, httpapi.Policy{
    Permission: "files:read", RateLimit: httpapi.RateLimitRelaxed,
}, handler)
```

The helper attaches the policy to `huma.Operation.Metadata`, adds the OpenAPI
security requirement, and builds the one correct middleware order:

```
rate limit (IP) -> authenticate -> rate limit (principal) -> authz -> MFA
  -> idempotency -> audit guard -> handler
```

The first limiter pass is per client IP, so a flood of bogus credentials cannot
drive unthrottled credential lookups. The second pass runs after authentication
and therefore keys the tier by user or API key; the response headers describe
that principal quota.

- A non-public operation must name a `Permission` or an `AnyOf` list.
- A `Public` operation is an explicit allowlist entry in `test/arch`.
- Any `platform:*` permission implies passkey MFA automatically, wherever the
  route is mounted: `RequireMFA` accepts `otp`/`rc`/`hwk`/`swk`, while a
  platform permission requires the phishing-resistant `hwk`/`swk` methods.
- `JWTOnly` rejects API keys (sessions, MFA factors, login-independent identity
  actions, destructive self-service).
- `NoStore` sets `Cache-Control: no-store`.
- `Audit` is `Tier1Action` (a service records the action in the mutation
  transaction) or `Guard{Action,Resource,Mode}` (the HTTP layer writes the
  required intent row). A mutation with neither fails `test/arch`.
- `test/arch` walks the registered operations and enforces: public-or-permission,
  mutation audited, platform permission => MFA, unique method+path and
  operationId, canonical path grammar, and the custom-method syntax.

## JSON conventions

| Rule | Example |
|---|---|
| camelCase keys | `accessToken`, `nextCursor` |
| IDs are UUIDv7 strings (RFC 9562); timestamps are RFC 3339 UTC strings | `"createdAt": "2026-10-24T12:00:00Z"` |
| Optional values are omitted or `null`; empty lists are `[]`, never `null` | `"data": []` |
| Requests reject unknown fields and trailing JSON values | `{"name":"x","extra":1}` is rejected |
| Bodies are `application/json`; uploads are `multipart/form-data` | `POST /files` |

## Success responses

| Status | Body |
|---|---|
| `200 OK` | The resource directly (no `{data,meta}` wrapper) |
| `201 Created` | The created resource and a `Location` header |
| `202 Accepted` | `Location` of the status resource for durable work (exports, erasure, replay) |
| `204 No Content` | No body (deletes, logout, non-enumerating request endpoints) |
| List | Cursor envelope below |

```json
{
  "object": "list",
  "url": "/api/v1/files",
  "data": [],
  "hasMore": false,
  "nextCursor": null
}
```

The envelope is `pagination.List[T]`; handlers construct it with
`pagination.NewList` (or `httpapi.ListOutput[T]`). `httpapi.NewCreated` and
`httpapi.NewAccepted` set the status and `Location`; `httpapi.Empty` is the
204 body. There is no `{"message": ...}` success body.

## Pagination

Forward-only cursor pagination: `limit` (1-100, default 20; non-positive becomes
20, above 100 becomes 100) plus `startingAfter`. Cursors are opaque base64url of
`<created_at RFC3339Nano>:<id>`. Repositories use `(created_at, id)` indexes and
the row comparison `(created_at, id) < (@cursor_created_at, @cursor_id)`; never
the `NOT @has_cursor OR ...` form, which turns deep pages into a scan. Offset
helpers stay limited to small admin/export tables.

## Update semantics

| Style | Routes |
|---|---|
| **Partial update** — `PATCH` with `application/merge-patch+json` (RFC 7396) | `/me/profile`, `/members/{id}`, `/roles/{id}`, `/webhooks/...`, `/platform/tenants/{id}`, `/platform/feature-flags/{key}` |
| **Full replace** | None for config resources; every config field is a merge-patch member. Point-of-no-return state transitions use their state field (`status`, `enabled`) in `PATCH`. |
| **Upsert** | `PATCH /me/notification-preferences` (one preference) and `POST /platform/feature-flags/{key}/targets` |
| **Create** | `POST /collection` (multipart `POST /files`, file upload) |
| **Custom method** | `POST /resource/{id}/verb` only where no state field fits: `restore`, `rotate-secret`, `replay`, `step-up`, and the auth ceremonies (`complete`, `verify`, `accept`, `signup`, `exchange`) |

`pkg/patch.Field[T]` is the reusable DTO member: absent = unchanged, explicit
`null` = clear, value = replace. `pkg/patch.Merge` applies the same semantics to
free-form JSON objects.

## Conditional requests

Tenant roles, feature flags, webhook endpoints/sources, and tenants are config
resources:

- A read or update returns `ETag`.
- An update may send `If-Match`; a stale value is `412
  resource.precondition_failed`. A missing `If-Match` is unconditional.
- The validator is the exact column the UPDATE compares against. Every
  conditional mutation executes one statement such as
  `UPDATE ... WHERE id =  AND tenant_id =  AND updated_at = `; 0 rows
  is `412` when the row exists and `404` when it was deleted concurrently.
  There is no read-compare-write window, so two writers holding the same ETag
  cannot both succeed. `httpapi.CheckIfMatchValue` is the header parser/early
  check; the database statement is the authority.

## Browser refresh-token transport (BFF)

`AUTH_REFRESH_COOKIE=true` moves the refresh token out of JSON into
`__Host-refresh` (HttpOnly, Secure, SameSite=Strict, Path=/, no Domain). The
operations that issue a pair (login, refresh, MFA verify/confirm, passkey
login, Google exchange) set it through a `Set-Cookie` response header and leave
`refreshToken` empty. `POST /auth/refresh` and `POST /auth/logout` accept the
cookie (the JSON body is optional) and enforce CSRF: `Sec-Fetch-Site`
same-origin/same-site passes; `cross-site`/`none` is refused; otherwise a
trusted `Origin` plus `X-CSRF-Token` is required. A refusal is `403
auth.csrf_failed`. Logout expires the cookie. JSON mode (the default) never
sets it and keeps the token in the body. The OAuth flow cookie is
`__Host-oauth_state`.

## Client metadata and login lockout

The platform records the trusted-proxy-aware client IP and User-Agent in the
request context once, before any handler. Public credential operations (login,
password-reset request, passkey login, MFA verify) use that value for the
per-(email, IP) lockout budget, session rows, and audit metadata. Five failures
from one IP lock only that source; the account-wide cap is 100 consecutive
failures, so a handful of addresses cannot lock a victim out. Neither key is
tenant-scoped.

## Feature-gate policy

A route may declare `Policy.Feature`; the shared middleware chain evaluates it
after authentication/authorization (so tenant and user targets apply) and
before `NoStore`, `Idempotency`, and the audit Guard. A disabled flag therefore
returns `404 feature.disabled` without taking an idempotency lock or writing an
audit intent row. Handlers do not re-check a gate that the policy owns; only
dynamic decisions (the Google callback tenant from signed state, the
email-verification requirement) stay in the service/handler.

## Errors (RFC 9457)

All errors use `application/problem+json` with the same shape for Fiber
middleware errors and huma handler/validation errors:

```json
{
  "type": "/errors/resource-not-found",
  "title": "Not Found",
  "status": 404,
  "code": "resource.not_found",
  "detail": "file not found",
  "instance": "/api/v1/files/0192...",
  "requestId": "...",
  "timestamp": "2026-10-24T12:00:00.000Z",
  "errors": [{"path": "name", "code": "required", "message": "..."}]
}
```

| Rule | Detail |
|---|---|
| `code` | Stable dotted catalog identifier. Generic statuses have exactly one code: `request.invalid`, `request.validation_failed`, `request.payload_too_large`, `request.unsupported_media_type`, `request.method_not_allowed`, `request.not_acceptable`, `request.canceled`, `auth.unauthenticated`, `permission.denied`, `resource.not_found`, `resource.conflict`, `resource.gone`, `resource.precondition_failed`, `rate_limit.exceeded`, `system.internal`, `system.timeout`, `system.unavailable`, `system.bad_gateway`, `system.audit_unavailable`. Module-specific conditions keep their module code. |
| `detail` | Public text only. A 5xx logs `slog.ErrorContext` with the internal cause (`AppError.Cause`/`Unwrap`) and never returns it. Use `apperror.Internal(err, "public text")`. |
| `type` | `ERROR_DOCS_BASE_URL` + the catalog `DocPath`. |
| Validation | `422 request.validation_failed` with a flat `errors[]`; huma's validation details are mapped into the same list. |
| Localization | `title`, `errors[].message`, and opted-in `detail` follow `Accept-Language` (`en`/`vi`). `code`, `type`, `status`, `instance`, and paths never change. |
| Authorization | `403 permission.denied`. MFA re-auth is `403 auth.mfa_required`. |
| Existence | Never disclose another user's or tenant's resource; return `404 resource.not_found`. |

### Status codes

| Code | Meaning |
|---|---|
| 200 | Read/update success |
| 201 | Resource created (with `Location`) |
| 202 | Durable work accepted (with `Location` of its status resource) |
| 204 | Deleted/no content |
| 400 | Malformed request, invalid cursor, invalid token |
| 401 | Missing/invalid authentication |
| 403 | Authenticated but not permitted, tenant mismatch, insufficient rank, or MFA required |
| 404 | Not found (including non-owned or cross-tenant resources) |
| 406 | No acceptable response representation (content negotiation) |
| 409 | Conflict, duplicate, last-owner, tenant deletion not due, export not ready, idempotency key in progress |
| 410 | Expired data export |
| 412 | Stale `If-Match` |
| 413 | Body or upload too large |
| 415 | Unsupported media type |
| 422 | Validation failed, or `Idempotency-Key` reused with a different body |
| 429 | Rate limited |
| 499 | Client closed the request (logged; usually never delivered) |
| 500 | Internal error |
| 503 | Dependency degraded/unavailable, or required audit intent unavailable |
| 504 | Request timeout |

## Authentication and authorization

Every protected operation runs the same chain, in this order:

1. the route's rate-limit tier;
2. `Authenticate` (API key or JWT) or `AuthenticateJWT` when the policy is
   JWT-only; both include the per-IP auth limiter;
3. `authzhttp.Require`/`RequireAny`;
4. `authzhttp.RequireMFA` when the policy requires it (always for `platform:*`);
5. idempotency, audit guard, handler.

The JWT carries user id, `tenant_id`, `sid`, `token_version`, and optional `amr`
(`pwd`/`google`/`otp`/`rc`); it never carries a role. The session snapshot
supplies the effective permission set for 5 seconds, and the session must be
live, the membership and user active, the tenant operational, and the token
version current. API-key scopes are intersected with the owner's current
effective permissions. An explicit `X-Tenant-Slug` that conflicts with the JWT
claim or key tenant is `403 permission.denied`.

JWT-only operations are listed in [api-routes.md](api-routes.md) (`JWT-only`):
session management, MFA factor/passkey management, invitation acceptance, the
caller's erasure, and platform MFA reset. A destructive admin erasure is
JWT-only as well.

Object-level checks use `authz.Subject` (explicit permissions, tenant, owner,
cross-tenant flag, attributes, `amr`) with `Policy`/`PolicySet`,
`TenantPolicy`, and `OwnerPolicy`. `Authorize` runs an RBAC pre-check, then
deny-overrides evaluation: deny is `403 permission.denied`, conceal is `404`.
With no subject, only `authz.SystemContext` is allowed; any other subject-less
call is `403`.

## MFA

TOTP (RFC 6238, SHA-1, 6 digits, 30 s, ±1 step), recovery codes, and WebAuthn
passkeys are per global user. Factor management lives under `/me/mfa`; the login
ceremonies live under `/auth`.

| Route | Behavior |
|---|---|
| `GET /me/mfa` | Status and factors; recovery-codes remaining. |
| `POST /me/mfa/totp` | Start/restart a pending enrollment, return the `otpauth://` URI once. Re-enrollment replaces the secret and resets the 15-minute window. Re-authenticate with `currentPassword` or a fresh passwordless session. |
| `POST /me/mfa/totp/{factorId}/confirm` | Verify the pending secret; enable the factor, issue ten single-use recovery codes, bump `token_version`, and re-issue the caller's session/tokens once. |
| `DELETE /me/mfa/{factorId}` | Require a current TOTP/recovery code; delete the factor and codes, revoke other sessions. |
| `POST /me/mfa/recovery-codes` | Require a code; return ten new codes and invalidate the old set. |
| `POST /me/mfa/step-up` | Refresh `sessions.mfa_verified_at` with a current TOTP/recovery code or a passkey assertion; used when an MFA-gated route returns `403 auth.mfa_required` for staleness. A passkey step-up also adds `hwk`/`swk` to the session's `amr`. |
| `POST /me/mfa/passkeys/options`, `POST /me/mfa/passkeys`, `DELETE /me/mfa/passkeys/{passkeyId}` | WebAuthn registration ceremony and factor lifecycle. Options are JWT-only and `No-store`; the attestation is verified with user verification required and stored by credential id (global to the user). |
| `POST /auth/mfa/verifications` | Public: single-use, tenant-bound, five-minute challenge plus TOTP/recovery code → normal tokens. |
| `POST /auth/passkeys/options`, `POST /auth/passkeys/verifications` | Public WebAuthn login ceremony. Options are discoverable and never reveal whether an email hint exists; a verified assertion issues the normal token pair with `hwk`/`swk` and a fresh `mfa_verified_at`. |
| `DELETE /platform/users/{userId}/mfa` | Platform recovery: delete factors and recovery codes, revoke every session, bump `token_version`, Tier 1 audit. |

A confirmed TOTP factor makes password and Google login return `200` with
`mfaRequired: true` and `mfaChallengeToken` instead of tokens. Passkeys are a
separate, phishing-resistant path: a passkey login issues tokens directly and
records `hwk`/`swk`. Every
token-minting and secret-returning MFA route is `No-store` and never takes
`Idempotency-Key`. Second-factor guesses use a per-user budget (5 per 15
minutes) that only a successful verification clears; failures are audited with
a reason class only. `authzhttp.RequireMFA(maxAge)` requires `otp`/`rc`/`hwk`/
`swk` in `amr` and `mfa_verified_at` no older than `MFA_MAX_AGE` (default 12h).
`authzhttp.RequirePasskeyMFA(maxAge)` is the stricter platform gate: only
`hwk`/`swk` satisfy it, so a TOTP or recovery code cannot reach a cross-tenant
platform operation. A passkey login or step-up records `hwk` (device-bound) or
`swk` (backup-eligible) per RFC 8176 and sets `mfa_verified_at`; a signature
counter regression is treated as a cloned authenticator and rejected.

## Tenants and members

`X-Tenant-Slug` (lowercase `[a-z0-9-]{1,63}`) selects the tenant on `/api/v1`
routes. Without it, local/test mode resolves `TENANT_DEFAULT_SLUG` (`default`);
`TENANT_REQUIRE_HEADER=true` rejects header-less requests and is required in
production. A validated `?tenant=<slug>` fallback remains only on the Google
redirect and password-reset/email-verification links; the header wins. The
Google callback and the public webhook receiver resolve their tenant from the
signed state/source row instead.

| Rule | Behavior |
|---|---|
| Unknown/suspended tenant | `404 resource.not_found`; startup checks only existence, so a suspended default tenant does not crash the process. |
| Scoping | Users are global; membership is the tenant join. Login needs an active membership in the request tenant. Cross-tenant or non-member ids are `404`. |
| Member administration | `PATCH /members/{memberId}` carries `roleIds` and/or `status` (`active`, `suspended`, `removed`; a ban is `suspended` plus `bannedAt`). `DELETE /members/{memberId}` removes the membership. Ban/status/role changes revoke the member's sessions in that tenant. |
| Role hierarchy | `owner` (3) > `admin` (2) > `member`/tenant role (1). Granting or inviting a role needs caller rank >= the role's rank and every permission it grants. Managing a member needs strictly higher rank (owners may manage co-owners). Failures are `403 role.insufficient_rank`. |
| Last owner | The last active system owner of an operational tenant cannot be demoted, banned, removed, or deleted. |
| Invitations | `POST /invitations` emails a token that is never returned; `POST /auth/invitations/accept` (JWT) and `POST /auth/invitations/signup` (public, token possession) join the tenant. Re-inviting a pending address replaces the token. |
| Platform | `platform:*` permissions are granted only by a `platform_admin` assignment, are mounted under `/platform`, and always require MFA. A tenant admin without the assignment gets `403`. |
| Tenant lifecycle | `PATCH /platform/tenants/{tenantId}` with `{status}` drives `trialing`, `active`, `suspended`, `pending_deletion`, `deleted`. `pending_deletion -> deleted` before the grace period is `409 tenant.deletion_not_due`; the hourly job performs due transitions. |

## Privacy: data export and erasure

| Route | Behavior |
|---|---|
| `GET /me/data-exports` | Cursor list of the caller's exports. |
| `POST /me/data-exports` | Queue one tenant-scoped export; `202` with `Location`; a second queued/running export is `409 data_export.in_progress`. |
| `GET /me/data-exports/{exportId}` | Status (`queued`, `running`, `ready`, `failed`, `expired`) and `downloadUrl` when ready. |
| `GET /me/data-exports/{exportId}/content` | Stream the ready JSON document, `Cache-Control: no-store`; `409` not ready, `410` expired, `404` unknown/other owner. |
| `POST /me/erasure-requests` | Self erasure: re-authenticate, reserve/suspend every membership, revoke sessions, queue one erase job per membership plus the global finalizer; `202` with `Location`. Refused while it would leave a tenant without an active owner (`409 user.last_admin`). |
| `GET /me/erasure-requests[/{requestId}]` | Status resource for the accepted erasure. |
| `POST /members/{memberId}/erasure-requests` | Tenant-scoped admin erasure; JWT-only, rank checked, one erase job for this tenant, no finalizer; `202` with `Location`. |

The export document is `{userId,tenantId,generatedAt,sections:{identity,files,
notifications,apikeys,features,webhooks}}`; it never includes secrets or
hashes. Blobs are embedded up to 64 MiB, later files carry
`contentOmitted:true`. Exports expire after 7 days; the hourly
`privacy.exports.purge` removes the blob then the row. Self erasure anonymizes
the global user only after every membership is removed; audit rows keep ids and
outcomes only.

## Roles, feature flags, and platform administration

Tenant roles are rows; the permission catalog stays in code and is synchronized
into `permissions` at API startup. System roles (`owner`, `admin`, `member`) are
seeded, immutable, and visible to every tenant.

- `/roles` CRUD is `admin:roles:*`, Tier 1 audited, ETag/If-Match protected.
  System roles cannot be changed or deleted; an assigned role cannot be deleted.
- Feature flags are process-wide and live under `/platform/feature-flags` with
  `platform:features:read|write` (MFA required). Precedence: force-off > kill
  switch > user target > tenant target > database default > environment >
  compiled default. `killSwitch` fails closed while a snapshot is stale.
  Targets use `POST /platform/feature-flags/{key}/targets` to upsert and
  `DELETE .../{targetId}` to remove.
- Feature-flag reads require `platform:features:read`; writes accept
  `Idempotency-Key`, return/honour ETags, and are Tier 1 or Guarded.
- A route gated by a disabled flag returns `404 feature.disabled`.

## Files

- `POST /files` accepts multipart `file`, enforces the size and MIME allowlist,
  and stores blobs under `<tenantID>/<userID>/<uuid>.<ext>`. It is the only
  route allowed to parse a multipart body up to `APP_BODY_LIMIT`.
- Optional byte quotas (`FILES_TENANT_QUOTA_BYTES`, `FILES_USER_QUOTA_BYTES`)
  count live plus trashed files. A cheap pre-check rejects an already-over
  upload before the blob is written; the authoritative check runs inside the
  metadata transaction under a per-tenant advisory lock. Over quota is `413
  file.quota_exceeded`.
- With `FILES_SCAN_DRIVER=clamav`, the blob is scanned before commit; a match is
  `422 file.infected`, scanner failure is `503 file.scan_unavailable` (fail
  closed).
- `GET /files` is caller-scoped unless the subject holds `admin:files:read`;
  non-owned and cross-tenant ids are `404`. `GET /files/{fileId}/content`
  returns bytes with `Content-Disposition` (RFC 6266/5987).
- Deletes are soft. `includeDeleted=true` exposes trash to the owner;
  `POST /files/{fileId}/restore` requires `files:restore`.
- The hourly `files.orphans.sweep` deletes blobs older than an hour with no
  `files` row.

## Webhooks

| Area | Contract |
|---|---|
| Config | `/webhooks/endpoints` and `/webhooks/inbound-sources` are `webhooks:read|write`, ETag/If-Match protected, and return a plaintext secret only on create/rotate. Rotate accepts `{"expirePrevious":true}` to end co-signing immediately. |
| Audit | Create/update/delete, rotate, and replay are Tier 2: a required Guard intent is written after idempotency (`503 system.audit_unavailable` changes nothing). |
| Deliveries | `GET /webhooks/deliveries` filters `endpointId` and `status`; detail includes the payload snapshot, lists omit it. `POST /webhooks/deliveries/{deliveryId}/replay` creates a new delivery/event and returns `202` with `Location`. |
| Outbound | At-least-once Standard Webhooks: `webhook-id` (dedupe key), `webhook-timestamp`, `webhook-signature: v1,<base64>`, HMAC-SHA256 over `<id>.<timestamp>.<body>`. SSRF guard at create/update and dial time, no redirects, no proxy, pinned DNS, ports 443/80 plus `WEBHOOK_ALLOWED_PORTS`. |
| Retries | `408`, `425`, `429`, `5xx` retry; other `3xx`/`4xx` dead. `Retry-After` honored. |
| Inbound | `POST /webhooks/inbound/{sourceId}` is public: source HMAC, no tenant header or idempotency middleware. Bad/unknown/stale signatures all return the same `401`; malformed input `400`; new event `202`; duplicate `(source,webhook-id)` `200 {duplicate:true}`. The service records the Tier 1 `webhook.received` action. |

Full signing, retry, secret, and operations contract: [Webhooks](webhooks.md).

## API keys

- Keys are created/read/updated/deleted under `/me/api-keys`; an admin managing
  another member uses `/members/{memberId}/api-keys/{keyId}` and must outrank
  the owner.
- `POST /me/api-keys` and `POST .../rotate` return the plaintext key once,
  are `No-store`, and never take `Idempotency-Key`.
- Format: `fgb_<43 base62 chars of 32 random bytes><6 base62 chars of a CRC32
  checksum>`. A bad checksum is rejected before any database lookup; the hash is
  stored, never the key.
- `APIKEYS_MAX_LIFETIME` (default `8760h`) caps every key. An omitted
  `expiresAt` defaults to now + maximum; a later value is `400
  api_key.expiry_exceeds_max_lifetime`. Rotation inherits the earlier of the
  original expiry and now + maximum.
- Update/delete are Tier 1 audited with no secret or hash in metadata; create
  and rotate are JWT/owner-bound. Another owner's key is `404`; a non-subset
  scope change by a non-owner is `403 permission.denied`.

## Notifications

All routes are scoped to the authenticated user and request tenant; another
user's/tenant's id is `404`.

| Route | Behavior |
|---|---|
| `GET /me/notifications` | Cursor list, `unreadOnly=true`, list envelope. |
| `GET /me/notifications/unread-count` | `{count}`. |
| `GET /me/notifications/{notificationId}` | One owned notification. |
| `PATCH /me/notifications/{notificationId}` | Merge patch `{read}`; sets `read_at` once. |
| `PATCH /me/notifications` | `{read:true}` marks every unread row; `{updated}`. |
| `GET`/`PATCH /me/notification-preferences` | Explicit preferences; a missing row means both channels on. `type` matches `^[a-z0-9][a-z0-9._-]*$` (max 100); channels `in_app`, `email`. |
| `POST /members/{memberId}/notifications` | Admin-only `notifications:send`; recipient must be an active member. `201` with `notification` (null when in-app is disabled) and `emailQueued`; Guarded. |

The caller's own read/preference writes are Guarded best-effort and allowlisted
in `test/arch`; with email enabled the in-app row and durable
`email.send.encrypted` job commit together. Both channels disabled is a
successful no-op.

## Headers

| Header | Direction | Contract |
|---|---|---|
| `X-Request-Id` | in/out | Accepted from a trusted proxy only; otherwise generated and echoed. |
| `X-Tenant-Slug` | in | Selects the tenant; in the default CORS allow list. |
| `Idempotency-Key` | in | Opt-in on retryable mutations; 1-128 chars of `[A-Za-z0-9._:-]`. |
| `Idempotent-Replayed` | out | `true` on a replayed response. |
| `If-Match` | in | Optimistic concurrency for config resources; stale is `412`. |
| `Cookie: __Host-refresh` | in | BFF refresh token; refresh/logout only, and the body becomes optional. |
| `Set-Cookie: __Host-refresh` | out | Issued/rotated on token responses and expired on logout in cookie mode. |
| `Sec-Fetch-Site`, `Origin`, `X-CSRF-Token` | in | CSRF decision for a cookie-authenticated refresh/logout. |
| `ETag` | out | Config-resource version. |
| `Location` | out | Created resource (`201`) or status resource (`202`). |
| `Cache-Control: no-store` | out | Tokens, secrets, and export content. |
| `Accept-Language` | in | `en`/`vi`, q-values, wildcards, region truncation. |
| `Content-Language`, `Vary: Accept-Language` | out | Negotiated locale. |

`Location`, `ETag`, `Idempotent-Replayed`, locale metadata, the rate-limit
headers (`RateLimit`, `RateLimit-Policy`, `X-RateLimit-Limit`,
`X-RateLimit-Remaining`, `X-RateLimit-Reset`, `Retry-After`), and
`X-Request-Id` are exposed through CORS.

## Rate limits

- Configured via `RATE_LIMIT_*`; tiers are strict/normal/relaxed with separate
  keyspaces. Cache-backed (Redis in production; memory is single-replica only).
- The window is a sliding approximation (current window plus the previous,
  weighted by overlap and floored), so a client cannot double its allowance at
  a boundary. Rejected retries do not advance the counter. Responses carry
  `RateLimit-Policy`/`RateLimit` plus legacy `X-RateLimit-*`; `429` carries
  `Retry-After` pointing at the first second the caller fits again.
- Every protected operation runs a per-IP auth limiter before credential
  verification, so bogus credentials cannot drive unthrottled lookups.
- Login attempts, password-reset requests, and verification resends have
  dedicated lockouts keyed per (email, IP) and, after 100 consecutive failures,
  globally.

## Idempotency

| Situation | Response |
|---|---|
| First request with a key | Runs normally; a 2xx is cached for 24h |
| Same key, same body, after success | Replayed with `Idempotent-Replayed: true` (not re-audited) |
| Same key while the first is in progress | `409 resource.conflict` |
| Same key, different body (non-multipart up to 1 MiB) | `422 idempotency.key_mismatch` |
| Lock claim reply lost | Re-read the lock: own token runs, other token `409`, missing/read error fails open |
| Cache unavailable | Fails open (request runs without idempotency) |

Scope is method, route template plus parameter values, tenant ID/slug, caller
(user id or IP), and key. Token-minting and secret-returning operations never
take the header. Replays are not audited a second time.

## Audit and fail-closed behavior

- Privileged mutations are audited before or atomically with the write. Tier 1
  service audit failure rolls the mutation back and returns `503
  system.audit_unavailable` (retryable). A Tier 2 required Guard intent failure
  returns the same 503 before the handler.
- Retrying after 503 is safe with the same `Idempotency-Key` where supported.
- Audit metadata is sanitized: passwords, tokens, secrets, and raw URLs are
  never stored; client text is UTF-8 validated and bounded. No public endpoint
  echoes audit data.
- A completion failure after a Tier 2 mutation cannot undo the write; alert on
  `audit_completion_failures_total` and stale `started` rows.

## Compatibility checklist

| Change | Classification |
|---|---|
| New field, new endpoint | Additive |
| New enum value | Allowed if clients are tolerant; document it |
| New permission | Additive; grant it deliberately |
| New required request field | Breaking |
| Changed error code or status | Breaking |
| Changed route or custom-method syntax | Breaking |
| New migration | Fresh installs update the baseline; deployed DBs get `make migrate-create module=<owner> name=<purpose>` |
