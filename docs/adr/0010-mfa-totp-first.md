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
