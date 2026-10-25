# 0010. MFA: TOTP and recovery codes first

- Status: Accepted
- Date: 2026-10-25

## Context

Login issues tokens after a password or Google sign-in only. Enterprise
customers and [OWASP ASVS 5.0](https://owasp.org/www-project-application-security-verification-standard/) /
[NIST SP 800-63B-4](https://pages.nist.gov/800-63-4/sp800-63b.html) expect
multi-factor authentication, at least for administrators.

## Decision

Implement TOTP (RFC 6238) factors and hashed single-use recovery codes, with a
two-step login (`mfa_required` challenge) and an `amr` claim. WebAuthn/passkeys
follow later on the same `mfa_factors` model.

## Consequences

- Schema: `mfa_factors`, `mfa_recovery_codes`; secrets encrypted with the data
  encryption keyring.
- Platform-scope roles require MFA.

## Implementation notes (P2b-3)

- `mfa_factors` is per global user (ADR 0004), not per tenant: one confirmed
  TOTP factor and one pending enrollment (partial unique indexes), with
  `last_used_step` claimed by `UPDATE ... WHERE last_used_step < $step`.
  `mfa_recovery_codes` stores SHA-256 hashes; `used_at` is claimed the same way.
  Both guards make concurrent use of one code succeed at most once.
- The TOTP secret and the two-step login challenge are sealed with
  `jobs.PayloadCodec` (`DATA_ENCRYPTION_KEY` plus `DATA_ENCRYPTION_PREVIOUS_KEYS`),
  so previous keys still decrypt. The challenge is a sealed payload with a
  nonce whose cache entry is removed with `CompareAndDelete`: five-minute TTL,
  single use, tenant-bound, and never a token pair.
- `sessions.amr` persists the authentication methods set so a refresh keeps
  `otp`/`rc`; access tokens carry the same `amr`. Tokens without `amr` remain
  valid. `authzhttp.RequireMFA()` and `authz.Subject.AMR` are provided, but
  which roles require MFA is deferred with the RBAC rebuild (P2b-2).
- Confirming or disabling revokes the user's other sessions
  (`revoked_reason = 'mfa_changed'`); the session that made the change stays
  live. Recovery codes are regenerated only after proving a current code.
- Deferred: WebAuthn/passkeys (they will use the same `mfa_factors.type`),
  per-tenant MFA policy, and step-up authentication for individual routes.
