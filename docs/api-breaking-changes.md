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
- New API keys use `fgb_<base62 random><base62 CRC32 checksum>`; keys issued in
  the old base64url format still verify but are deprecated. Rotate them to get
  the current format.

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
