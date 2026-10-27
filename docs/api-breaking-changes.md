# Intentional breaking API changes

CI's `API compatibility` job compares the base branch's and the pull
request's `docs/openapi.yaml` (OpenAPI 3.1) with
`oasdiff breaking --fail-on ERR`. A pull request that **edits this file**
acknowledges its breaking changes: the job still prints the oasdiff report but
does not fail. Editing the file in an earlier PR does not exempt later ones.

Add one entry per intentional break, newest first: the date, the PR, every
affected `METHOD /path`, what clients must change, and the migration path.
Prefer a non-breaking alternative (new optional field, new route) before adding
an entry. Earlier entries below are historical and may mention routes that the
2026-10-27 canonical remap replaced.

Reproduce the check locally:

```bash
go tool oasdiff breaking /tmp/base-openapi.yaml docs/openapi.yaml --fail-on ERR
```

or fetch the base file:

```bash
git show origin/main:docs/openapi.yaml > /tmp/base-openapi.yaml
```

## Entries

### 2026-10-27 — Webhooks converted to the canonical huma route map

- `PUT /api/v1/webhooks/endpoints/{endpointId}` and
  `PUT /api/v1/webhooks/inbound-sources/{sourceId}` are now `PATCH` with
  `Content-Type: application/merge-patch+json` (RFC 7396): omitted members are
  unchanged, `null` restores the default, and any other value replaces it. A
  full replacement must send every member it wants to keep.
- `POST /api/v1/webhooks/deliveries/{deliveryId}/replay` now returns `202` with
  `Location: /api/v1/webhooks/deliveries/{newId}` (was `201` with the delivery
  body and no Location). The new delivery is the status resource the Location
  names.
- Both `.../rotate-secret` operations return `200 {"secret": "..."}` only; the
  endpoint/source representation is no longer embedded in the response (read
  the resource's own GET). Secret-returning operations remain `Cache-Control:
  no-store` and are never idempotency-cached.
- Endpoint and inbound-source create accept an optional `status`
  (`active|disabled`); omitted means `active`. Copies of the resource are now
  ETag/If-Match protected (`412 resource.precondition_failed` on a stale
  validator), and the path parameters are `{endpointId}`/`{sourceId}` (the
  `{id}` spelling is gone).
- The public receiver `POST /api/v1/webhooks/inbound/{sourceId}` keeps its path
  and Standard Webhooks contract. Validation failures (unknown fields, invalid
  UUID, missing Standard Webhooks headers) are now `422
  request.validation_failed`; authentication failures remain one `401
  auth.unauthenticated`.
### 2026-10-27 — RFC 9068 access tokens, duration TTLs, and cookie mode

- Access tokens now carry `typ: at+jwt` and no longer carry an `email` claim.
  `pkg/token` rejects a token without `typ: at+jwt`, so every token issued
  before this release is invalid; clients must sign in again. Clients that
  read `email` from the token must call `GET /api/v1/users/me`.
- `JWT_EXPIRE_HOUR` is replaced by `JWT_ACCESS_TTL` (Go duration, default
  `15m`), and `JWT_REFRESH_EXPIRE_DAYS` by `JWT_REFRESH_TTL` (default `720h`).
  Deployments must rename both settings; old values are ignored.
- `AUTH_REFRESH_COOKIE=true` is a new browser transport: `POST /auth/login`,
  `/auth/refresh`, `/auth/mfa/verify`, `/auth/mfa/totp/confirm`, and
  `/auth/google/exchange` set the refresh token in the HttpOnly
  `__Host-refresh` cookie and return an empty `refreshToken` in JSON. Refresh
  and logout read the cookie; a cookie-authenticated request must be
  `same-origin`/`same-site` (`Sec-Fetch-Site`) or carry a trusted `Origin` plus
  `X-CSRF-Token`, else `403 auth.csrf_failed`. `POST /auth/refresh` and
  `/auth/logout` accept an empty body in cookie mode. JSON mode is unchanged.
  The `__Host-` prefix requires `Path=/` and no `Domain` (a browser rejects a
  narrower scope), so the cookie is readable by every API path but is only
  consumed by refresh and logout.
- The OAuth state cookie is renamed from `oauth_state` to
  `__Host-oauth_state`; a sign-in started before the deploy cannot complete.
  `OAUTH_STATE_KEY` is a new required setting when Google sign-in is
  configured.
### 2026-10-27 — Platform routes require a passkey (phishing-resistant AAL2)

- Every operation that accepts a `platform:*` permission now requires a
  WebAuthn passkey proof (`amr` contains `hwk` or `swk`); a TOTP or recovery
  code no longer satisfies the check and returns `403 auth.mfa_required`.
  Platform administrators must register a passkey through
  `POST /api/v1/me/mfa/passkeys/options` + `POST /api/v1/me/mfa/passkeys`
  before using `/platform` routes. TOTP remains valid for tenant-level
  MFA-gated routes.
- `POST /api/v1/me/mfa/step-up` accepts either `{"code": "..."}` as before or
  `{"passkey": <PublicKeyCredential>}`; the assertion challenge comes from
  `POST /api/v1/auth/passkeys/options`. A passkey step-up adds `hwk`/`swk` to
  the session.
- New passkey login routes (`POST /api/v1/auth/passkeys/options`,
  `POST /api/v1/auth/passkeys/verifications`) and passkey registration routes
  are additive. Passkey login returns the normal token pair and never asks for
  a TOTP challenge.

### 2026-10-27 — huma/OpenAPI 3.1 foundation and canonical route remap

- The HTTP layer is generated from huma operations. `docs/swagger.json`,
  `docs/swagger.yaml`, `docs/docs.go`, `cmd/openapi3`, `swag`, and the
  `/swagger` UI are removed; the spec lives in `docs/openapi.yaml` and is served
  at `/openapi.json`, `/openapi.yaml`, and `/openapi-3.0.{json,yaml}`.
  Clients that fetched the Swagger document must fetch `openapi.yaml` instead.
- `/api/v2`, `app.V2Module`, `Accept-Version`, `API-Version`, `Vary:
  Accept-Version`, `Deprecation`, `Sunset`, and the version `Link` headers are
  removed. Every operation is under `/api/v1`; unsupported version-shaped paths
  are a plain `404 resource.not_found`.
- Routes are remapped to the canonical table in
  [api-routes.md](api-routes.md): `/admin/*` becomes `/me/*`, `/members/*`,
  `/roles`, `/files`, `/stats`, `/webhooks/...`, or `/platform/...`. Notable
  moves: `PUT /admin/users/:id/role` and `PUT /admin/users/:id/roles` become
  `PATCH /members/{memberId}` with `{roleIds}`; `POST .../ban` and `/unban`
  become `PATCH /members/{memberId}` with `{status}`; `/admin/api-keys` is
  `/me/api-keys`; `POST /auth/logout-all` is `DELETE /me/sessions`;
  `/admin/features/*` is `/platform/feature-flags/*`; `POST /files/upload` is
  `POST /files`; `GET .../download` is `GET .../content`; and platform MFA reset
  is `DELETE /platform/users/{userId}/mfa`.
- Error codes are namespaced and one code means one thing:
  `bad_request`→`request.invalid`, `unauthorized`→`auth.unauthenticated`,
  `forbidden`→`permission.denied`, `not_found`→`resource.not_found`,
  `conflict`→`resource.conflict`, `internal_error`→`system.internal`,
  `payload_too_large`→`request.payload_too_large`,
  `unsupported_media_type`→`request.unsupported_media_type`,
  `validation_failed`→`request.validation_failed`,
  `too_many_requests`→`rate_limit.exceeded`,
  `gateway_timeout`→`system.timeout`,
  `client_closed_request`→`request.canceled`. The wire shape is unchanged.
- Responses follow the new envelope rules: `201`/`202` carry `Location`, no-body
  operations are `204`, lists use `pagination.List[T]`, token/secret/export
  responses are `Cache-Control: no-store`, partial updates are `PATCH` with
  `application/merge-patch+json`, and config resources use `ETag`/`If-Match`
  (`412 resource.precondition_failed`).
- There is no deployed client or database, so there is no compatibility window.
- Notifications and feature flags follow the same remap: the member feed is
  `/me/notifications` (read state is a `PATCH .../{notificationId}` merge patch
  instead of `POST .../read`), the feed-wide read is `PATCH /me/notifications`,
  preferences are `/me/notification-preferences/{type}/{channel}`, and the admin
  send is `POST /members/{memberId}/notifications`. Feature-flag targets are
  addressed by canonical item ids (`tenant`, `user:{userId}`,
  `tenant:{tenantId}`, `user:{tenantId}:{userId}`) instead of a surrogate UUID.

### 2026-10-27 — Every identifier is a UUIDv7

- Every resource identifier in every response (primary keys, `tenantId`,
  `userId`, `sessionId`, parent/replay references, webhook event and delivery
  ids, job ids, and the ids embedded in domain-event payloads) is now a UUID
  string instead of a decimal integer. Path parameters (`/users/{id}`,
  `/files/{id}`, `/tenants/{id}`, `/webhooks/{id}`, ...) accept only a UUID;
  a numeric id is `400 invalid id` (or `404` where the route conceals
  existence). Clients must treat ids as opaque strings and store them as such
  (a 128-bit UUID does not fit a 64-bit integer column). There is no v2 route:
  every module is part of the same fresh-install schema, so there is no
  deployed database or client that can keep the integer form.
- API-key request/response fields are unchanged in shape, but `expiresAt` is
  now required on creation (every key must expire), and API-key hashes are
  stored as 32-byte digests rather than hex text. Existing keys cannot be
  carried over because no deployment predates this schema.
- Notification preference responses (`GET/PUT /api/v1/notification-preferences`)
  no longer carry an `id`: the preference row is keyed by the natural key
  `(tenantId, userId, type, channel)`, so clients address it by `type` and
  `channel` and must stop storing or expecting `id`. Request bodies are
  unchanged. There is no v2 route: the surrogate id never identified anything
  the caller chose, and no deployment predates the natural key.

### 2026-10-26 — One format per credential; legacy acceptance paths removed

- API keys (`Authorization: ApiKey <key>`): only the checksummed
  `fgb_<43 base62 random><6 base62 CRC32>` format verifies. The retired
  `fgb_<32-byte base64url>` format now returns `401 invalid API key` before any
  lookup, so a legacy key cannot even rotate itself. A client holding one must
  have an admin create a replacement key (or use JWT) and switch to it.
- JWT access tokens: a token without the `kid` header is now `401 invalid or
  expired token`, so every client must sign in again to obtain a token issued
  by this release. The legacy `role` claim is no longer parsed: authorization
  always comes from the session's permission snapshot. There is no v2 route;
  accepting kid-less or role-bearing tokens would keep tokens whose signing key
  and authorization cannot be attributed.
- Webhook secrets: a stored value that is not `whsec_<base64>` is now a
  structural error. An outbound delivery dead-letters with
  `secret snapshot is not a Standard Webhooks secret`, and an inbound source
  rejects every request with the same `401` as a bad signature. There is no
  pre-Standard format to support and no database predates it; rotate any such
  row if one is ever found.

### 2026-10-26 — MFA confirmation re-issues the caller's tokens

- `POST /api/v1/auth/mfa/totp/confirm`: the response keeps
  `recoveryCodes` and now also carries `accessToken` and `refreshToken`.
  Confirmation bumps the user's token version so every session that existed
  before the factor was confirmed is invalid in every tenant (this closes the
  login race where a password-only sign-in slipped past an MFA enable). The
  session that confirmed the factor is re-issued and stays signed in, but its
  previous access and refresh tokens no longer work. Clients must replace
  their stored tokens with the pair in this response; ignoring it logs the
  caller out on the next request. There is no v2 route: keeping the old token
  pair valid would also keep a password-only login that raced the confirmation
  usable.

### 2026-10-26 — Webhook SSRF checks at configuration, HTTPS default, and API-key rank management

- `POST /api/v1/webhooks/endpoints` and `PUT /api/v1/webhooks/endpoints/{id}`:
  the URL hostname is now resolved during validation (3s timeout). A host that
  resolves only to a blocked/private address returns `400` instead of being
  accepted and failing every delivery. In production `WEBHOOK_REQUIRE_HTTPS`
  defaults to `true` and `http://` URLs are `400`; an operator can set it to
  `false` for a deliberate plaintext receiver. Clients using a public HTTPS
  receiver need no change.
- `POST /api/v1/webhooks/endpoints/{id}/rotate-secret` accepts an optional body
  `{"expirePrevious": true}` (default false). When true the previous secret
  stops co-signing new deliveries immediately instead of after the 24h grace.
  Requests without a body keep the previous behavior.
- `PUT /api/v1/admin/api-keys/{id}` and `DELETE /api/v1/admin/api-keys/{id}`
  (and its `POST .../revoke` alias): managing another member's key now requires
  the caller to outrank the key owner under the identity role hierarchy, so an
  admin can no longer change or revoke an owner's key (`403
  role.insufficient_rank`), and a member can no longer manage a peer's key.
  Only the key owner may widen its scopes; another manager may only narrow or
  revoke (`403 permission.denied`). Clients should use an owner/outranking
  admin account for these operations.
- `POST /api/v1/admin/api-keys/{id}/rotate` on an expired key now returns `409`
  instead of issuing an already-expired successor.
- Stored webhook secrets from the pre-v3 envelope formats are rejected rather
  than opened without tenant/entity binding. No database has been deployed, so
  there is no client or data migration; a pre-v3 ciphertext must be re-sealed
  (rotate the endpoint/source secret) if one exists.

### 2026-10-26 — Feature-flag writes require a second factor

- `PUT`/`DELETE /api/v1/admin/features/flags/{key}` now run
  `authzhttp.RequireMFA()` before the permission check because the route also
  accepts `platform:features:write`. A caller presenting only
  `admin:features:write` (a tenant admin) and a session without `otp`/`rc` in
  `amr` now receives `403 auth.mfa_required` instead of applying the change.
  Clients must complete MFA for the admin session, or use the platform
  permission (which already required a second factor). The target routes
  (`/flags/{key}/targets`) are tenant-only and unchanged. There is no v2 route.

### 2026-10-26 — Tenant deletion restore and erasure reservation

- `PATCH /api/v1/platform/tenants/{id}/status` no longer accepts `active` for a
  `pending_deletion` tenant; it returns `409` because the tenant may have been
  `suspended` before the deletion was requested. Clients must cancel a deletion
  with `suspended` and then activate the tenant explicitly with a second
  `active` transition. There is no v2 replacement: restoring straight to
  `active` could silently lift a suspension.
- `POST /api/v1/me/erasure` now suspends the caller's memberships and revokes
  their sessions in the request transaction. A second owner's request, whether
  sequential or concurrent, now returns `409 user.last_admin` instead of
  accepting and failing later; clients should surface that as "another owner
  must remain". The same reservation applies to
  `POST /api/v1/admin/users/{id}/erasure` for the request tenant only. The
  response shape is unchanged.

### 2026-10-26 — Padded common passwords, Google refusals, and AAL2 freshness

- `POST /api/v1/auth/register`, `POST /api/v1/auth/reset-password`,
  `POST /api/v1/auth/invitations/signup`, and `PUT /api/v1/users/me/password`
  now also reject a password whose leetspeak/punctuation-padded core is on the
  bundled common-password list (`P@ssw0rd2026`), a single repeated character,
  or a short repeated pattern (`abcabcabcabc`). The new field codes are
  `password_repeated` and (when the operator enables
  `PASSWORD_BREACH_CHECK=hibp`) `password_breached`; expose them alongside the
  existing password codes. Values are NFKC-normalized before length checks and
  before bcrypt on both set and verify, so compatibility spellings compare
  equal. That changes the hashing input: a hash written by an older build must
  be re-set (there is no deployed database yet). No v2 route is needed: the
  old policy accepted weaker secrets.
- `GET /api/v1/auth/google/callback` now returns `403
  auth.oauth_email_not_verified` when Google authenticated an account with an
  unverified address, and `400 auth.oauth_userinfo_incomplete` when the
  userinfo response lacks the subject id or email. Both were `502
  auth.oauth_upstream_unavailable`; clients should send the user back to
  Google to verify the address or retry the sign-in instead of showing a
  provider outage.
- MFA-gated routes (every `/platform` route, including platform MFA reset)
  now require the session's `mfa_verified_at` to be no older than
  `MFA_MAX_AGE` (default `12h`). A stale session gets `403
  auth.mfa_required` with a detail pointing at the new JWT-only `POST
  /api/v1/auth/mfa/step-up` (`{"code": "<TOTP or recovery code>"}` →
  `{"verifiedAt": "..."}`). Clients that cached a platform token across a
  shift must step up before the next privileged call; the token's `amr` claim
  is unchanged. There is no v2 route: the longer acceptance window was the
  vulnerability.

### 2026-10-26 — Webhook port policy, inbound enumeration, and API-key lifetime

- `POST /api/v1/webhooks/inbound/{id}`: an unknown, disabled, or soft-deleted
  source (or a suspended tenant) now returns `401 webhook signature is invalid`
  instead of `404`. All signature failures use that same problem, so clients
  cannot tell whether an id exists. Senders must keep using a valid source id
  and secret; there is no v2 route.
- `POST /api/v1/webhooks/endpoints` and `PUT /api/v1/webhooks/endpoints/{id}`:
  a URL whose port is not 443/80 (or an operator-allowed
  `WEBHOOK_ALLOWED_PORTS` entry) now returns `400 webhook.port_not_allowed`.
  Delivery also enforces the same policy at dial time. Clients using a
  receiver on another port must have the operator add it to
  `WEBHOOK_ALLOWED_PORTS`; there is no client-side override.
- `POST /api/v1/admin/api-keys`: an omitted `expiresAt` is now recorded as
  now + `APIKEYS_MAX_LIFETIME` (default `8760h`), and an `expiresAt` later than
  that maximum returns `400 api_key.expiry_exceeds_max_lifetime`. Clients that
  relied on non-expiring keys must omit the field (bounded default) or send an
  earlier expiry. Existing rows with NULL expiry keep working until rotated or
  revoked.
- New API keys use `fgb_<base62 random><base62 CRC32 checksum>`; the old
  base64url format was accepted during a deprecation window and is now rejected
  (see the newest entry).

### 2026-10-26 — Google sign-in returns a one-time code, not tokens

- `GET /api/v1/auth/google/callback`: the redirect fragment changes from
  `#access_token=…&refresh_token=…` to `#code=<one-time-code>`. The code is
  256 bits, single use, expires after 60 seconds, and is bound to the tenant in
  the signed `state`; browser history no longer records credentials.
- New public endpoint `POST /api/v1/auth/google/exchange` with
  `{"code": "<code>"}` and `X-Tenant-Slug` resolves the code to the normal
  login response (`accessToken`, `refreshToken`, `user`). An unknown, expired,
  already-redeemed, or other-tenant code returns `401 auth.oauth_code_invalid`.
- The authorization request now uses PKCE (S256) and no longer sends
  `access_type=offline`; this needs no client change.
- Clients must read `code` from the callback fragment and call the exchange
  endpoint instead of taking tokens from the fragment. There is no v2 route:
  the old fragment would keep credentials in browser history, which this change
  removes.

### 2026-10-26 — NIST 800-63B password policy

- `POST /api/v1/auth/register`, `POST /api/v1/auth/reset-password`, and
  `PUT /api/v1/users/me/password` now reject passwords shorter than 12
  characters, longer than 64 characters or 72 bytes, on the bundled
  top-10k common-password list, or containing the email local part.
  Composition rules (uppercase/lowercase/digit/special) are gone.
- Existing passwords keep working for sign-in; only a new password is
  validated. Clients should surface the `422 validation_failed` field codes
  `weak_password`, `password_common`, and `password_contains_email` and ask the
  user for a longer passphrase. No v2 route is needed: the old policy accepted
  weaker secrets and cannot be kept for new passwords.

### 2026-10-26 — API keys are owner-bound on create and rotate

- `POST /api/v1/admin/api-keys`: the `userId` request field is removed. A key is
  always owned by the authenticated caller. Requests that still send `userId`
  now fail with `400 bad_request` (the binder rejects unknown fields). Clients
  must change to: the member creates the key with their own session, or an
  administrator keeps using list/revoke to manage it.
- `POST /api/v1/admin/api-keys/{id}/rotate`: rotating a key owned by another
  member now returns `404` (previously it issued a secret for that member).
  Only the owner rotates. There is no v2 replacement: key rotation is
  self-service by design, because a secret must only ever reach its owner.
- Listing, getting, updating, and revoking another member's key in the same
  tenant remain available to administrators.

No client can keep the old behavior: use the owning member's session to create
or rotate their key.

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

### 2026-10-27 — Identity HTTP surface moved to the canonical route map

The identity module now serves only `/api/v1` through the huma operations in
`docs/api-routes.md`; the old Fiber route table is deleted with no aliases.

- Auth flows moved to plural resource paths:
  `POST /auth/forgot-password` → `POST /auth/password-resets`,
  `POST /auth/reset-password` → `POST /auth/password-resets/complete`,
  `POST /auth/resend-verification` → `POST /auth/email-verifications`,
  `POST /auth/verify-email` → `POST /auth/email-verifications/complete`, and
  `POST /auth/google/exchange` → `POST /auth/google/exchanges`.
- Password-reset and verification completions now return `204`; their old
  `{"message": ...}` bodies are gone.
- Profile, session, and MFA endpoints moved under `/me`:
  `GET/PUT /users/me` → `GET/PATCH /me/profile`,
  `PUT /users/me/password` → `POST /me/profile/password`,
  `GET /auth/sessions` → `GET /me/sessions`,
  `DELETE /auth/sessions/{id}` → `DELETE /me/sessions/{sessionId}`,
  `POST /auth/logout-all` → `DELETE /me/sessions`,
  `GET /auth/mfa/status` → `GET /me/mfa`,
  `POST /auth/mfa/totp/enroll` → `POST /me/mfa/totp`,
  `POST /auth/mfa/totp/confirm` → `POST /me/mfa/totp/{factorId}/confirm`,
  `POST /auth/mfa/disable` → `POST /me/mfa/{factorId}/disable`,
  `POST /auth/mfa/recovery-codes/regenerate` → `POST /me/mfa/recovery-codes`,
  and `POST /auth/mfa/step-up` → `POST /me/mfa/step-up`.
- Profile and role updates use RFC 7396 merge patch
  (`Content-Type: application/merge-patch+json`) instead of full-replace PUT.
  Tenant roles and members carry `ETag`; a stale `If-Match` is `412`.
- Members moved from `/admin/users` and `/users` to `/members`; role, ban, and
  unban became `PATCH /members/{memberId}` with `roleIds`/`status`. Invitations
  moved from `/invitations` (same path, new `{invitationId}` GET), roles from
  `/admin/roles` to `/roles`, and `/admin/stats` to `/stats`.
- Platform tenants moved to `/platform/tenants` with `PATCH {name?,status?}`
  and ETag; the old `PATCH /platform/tenants/{id}/status` is gone. A user's MFA
  reset is now `DELETE /platform/users/{userId}/mfa`.
- Data exports moved from `/me/data-exports/{id}/download` to
  `/me/data-exports/{exportId}/content`; erasure requests gained the status
  resource `GET /me/erasure-requests[/{requestId}]`.
- All 2xx responses keep problem+json errors, cursor list envelopes, and
  `Location` on 201/202; there is no v2 compatibility surface.

### 2026-10-27 — BFF refresh cookie, feature-gate policy, and SQL ETags

- `AUTH_REFRESH_COOKIE=true` is the browser transport. Token-pair responses
  (login, refresh, MFA verify/confirm, passkey login, Google exchange) set
  `__Host-refresh` (`HttpOnly; Secure; SameSite=Strict; Path=/; no Domain`) and
  return an empty `refreshToken`. `POST /auth/refresh` and
  `POST /auth/logout` accept that cookie with an optional JSON body and enforce
  CSRF: `Sec-Fetch-Site: same-origin|same-site`, or a trusted `Origin` plus
  `X-CSRF-Token`; otherwise `403 auth.csrf_failed`. Logout clears the cookie.
  The OAuth state cookie is `__Host-oauth_state`. JSON mode is unchanged and
  never reads a cookie. Clients that send a cookie cross-site must switch to a
  same-origin BFF call or send both fallback headers.
- A route-level feature gate now runs after authorization and before
  idempotency and audit. A disabled flag still returns `404
  feature.disabled`, but it no longer leaves an idempotency lock or an audit
  intent row (observable only to operators).
- `If-Match` is enforced by the conditional `UPDATE` itself for members, roles,
  tenants, feature flags, and webhook endpoints/sources. Two writers holding
  the same ETag now produce exactly one `200` and one `412` (previously both
  could pass the read-compare step). A missing `If-Match` stays unconditional.
- The public webhook receiver's `webhook.received` audit is now a required
  HTTP Guard intent (still one trail per received delivery), not a
  post-commit service event.
