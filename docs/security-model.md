# Security Model

This document states the threat model and the isolation and key-management
guarantees the code actually enforces. It is a boilerplate, not a certified
platform: the limits in [What is not guaranteed](#what-is-not-guaranteed)
matter as much as the guarantees.

Related: [Architecture](architecture.md), [Data Model](data-model.md),
[Operations](operations.md), [API Design](api-design.md),
[Webhooks](webhooks.md).

## Tenancy model: pool

The deployment is a **pool** model in the AWS SaaS Lens sense: one application
and one database serve every tenant; tenant rows share tables and are separated
by a `tenant_id` column, application predicates, and PostgreSQL row-level
security. It is not a silo (one database or stack per tenant) and not a bridge
(a mix of silo and pool). The choice is deliberate for a boilerplate: one
schema, one migration stream, one connection pool.

A tenant is a row in `tenants`; users are global identities that join tenants
through `memberships`. Every per-tenant table has `tenant_id` and a composite
foreign key to `tenants` or `(tenant_id, user_id) → memberships`, so a
cross-tenant reference is rejected by the schema as well as by queries. See
[Data Model](data-model.md).

| Principal | Reaches |
|---|---|
| Anonymous | Health/readiness/metrics endpoints (no tenant resolution) and the documented public routes. The tenant comes from `X-Tenant-Slug` or, only on `/api/v1/auth/google`, `/api/v1/auth/password-resets/complete`, and `/api/v1/auth/email-verifications/complete`, the validated `?tenant=` fallback; the Google callback uses signed state and the public webhook receiver uses its source row. |
| Tenant member | Only the request tenant's rows: the tenant comes from the JWT claim (after `X-Tenant-Slug` resolution and conflict checks) or from the API key, never from a request body. |
| Platform administrator | Named platform routes (`platform:tenants:*`, `platform:features:write`, `platform:users:mfa_reset`); these are the only cross-tenant HTTP capabilities. |
| Background worker / scheduler / seeder | `authz.SystemContext`; job handlers restore the job's `tenant_id`, and named global jobs mark a bypass context. |
| Database reader (dump, backup, replica) | Ciphertext/sealed data and pseudonymous markers; not tenant request paths. |

## Tenant isolation guarantees

### Request scope is set by the data layer only

Repositories are wired with `app.Infra.DB`, a `database.Scoped` handle, and
services never see `pgx.Tx` (`service/` may not import pgx). A transaction
starts in `database.TxManager.WithTx`, which runs

```sql
SELECT set_config('app.tenant_id', $1, true)
```

immediately after `BEGIN`; `database.Scoped` does the same in a single pgx
`Batch` (`BEGIN; set_config; statement; COMMIT`) for one statement. The third
argument (`true`) makes the setting transaction-local, so a pooled connection
is never left scoped to the previous caller. The value comes from
`tenant.IDFromContext`; a context without a tenant resolves to `0`/empty, and
`app_tenant_visible` compares against `NULL`, which is not true for any row
(fail closed). Repositories that must name a tenant on a row use
`tenant.EffectiveID`, but that compatibility fallback is never the RLS scope.

### Row-level security on every tenant-owned table

Every table with a `tenant_id` has `ENABLE` and `FORCE ROW LEVEL SECURITY` and
one `tenant_isolation` policy calling `app_tenant_visible(tenant_id)`.
`FORCE` applies the policy to the table owner too. `roles` additionally exposes
the shared system roles (`tenant_id IS NULL`) to every tenant for reading;
writing one requires the bypass. `cmd/migrate` discovers every table with a
`tenant_id` and refuses a schema where the policy is missing or a second policy
exists; `test/arch` and the migration verifier keep that check from drifting.

A query that forgot its `tenant_id` predicate still cannot read or write
another tenant's rows, and a `RETURNING` row from another tenant cannot leak.
The global tables (`users`, `user_passwords`, `user_identities`,
`password_reset_tokens`, `email_verification_tokens`, `mfa_factors`,
`mfa_recovery_codes`, `webauthn_credentials`, `platform_role_assignments`,
`tenants`, `permissions`, `feature_flags`, `scheduler_*`) are reached
through the caller's membership or are deliberately process-wide. Every other
table with a `tenant_id` — including `tenant_settings`, `roles`,
`memberships`, and each module's rows — is policed as above.

### Two database roles

Isolation is a **role property**, not a setting the application can flip:

- The normal API/worker/seed role is a non-superuser without `BYPASSRLS`. In
  production `cmd/api` refuses startup when the normal role is a superuser, has
  `BYPASSRLS`, owns a table in the configured schema, or is a member of such a
  role (so `SET ROLE` cannot reach one). It also requires
  `DB_SSLMODE=verify-full`.
- A dedicated `BYPASSRLS` role (`DB_BYPASS_USERNAME`/`DB_BYPASS_PASSWORD`) is
  used only by statements whose context was marked with
  `database.WithRLSBypass`. In production it must have `BYPASSRLS`, must not be
  a superuser or own tables, and the normal role must not be a member of it.

`database.Pools` chooses the pool at `BEGIN` (or per `Scoped` statement) from
the context. A bypass marker added **inside** a transaction that began on the
normal pool cannot move it, so those statements stay tenant-scoped (fail
closed). Outside production an unset bypass pair reuses the normal pool with a
warning; production validation requires the pair. The reviewed bypass paths are
only: job claim/lease recovery/retention, scheduler enqueue, global-user
account operations and credential-change session bookkeeping, retention purges,
feature snapshots, webhook inbound source lookup, tenant lifecycle/purge, API
key lookup and refresh-token revocation by possession, invitation
accept/sign-up possession lookup, and the ban-marker startup count. Each is one
named statement or transaction with an explicit tenant predicate where it
writes.

### Constant SQL and the allowlist ratchet

`test/arch/sql_literals_test.go` type-checks non-test code under `internal`,
`pkg`, and `cmd` and rejects any `Exec`/`Query`/`QueryRow`/`Batch.Queue` SQL
argument that is not a compile-time constant. A reviewed allowlist names the
few exceptions (the `Scoped` wrapper, schema verification, throwaway test
schema names) and may only shrink. `TestCodeSQLOnlyTouchesOwnedTables` rejects
module SQL that touches another owner's tables, including CTEs named like
them, and `test/arch` builds every module to check route authorization and
audit guards. Together these make a scope change injected through a dynamic
query fragment a code-review-visible event rather than a silent possibility.

## What is not guaranteed

- **A SQL-injection primitive that can run arbitrary SQL in a scoped
  transaction can read another tenant.** `app.tenant_id` is only a transaction
  setting; an injected statement could call `set_config('app.tenant_id',
  '<other>', true)` and the policy would follow. The constant-SQL rule above is
  the defense against reaching that primitive; it is not a second independent
  boundary. The database roles do not help here because the injected statement
  runs as the normal role inside the tenant's own transaction.
- **Silo isolation is out of scope.** Tenants share one database and one
  schema; there is no per-tenant database, cluster, or encryption key, so a
  database-level compromise affects every tenant.
- **Noisy neighbours are not isolated.** Per-tenant quotas/rate limits remain
  future work; a tenant can consume shared database connections, CPU, storage,
  and job-worker capacity. Application rate limits are per source/principal,
  not a tenant-wide fairness guarantee.
- **RLS is not a substitute for authorization.** It limits row visibility, not
  which columns or operations a member may use; permissions and
  object-level checks through the injected `authz.Authorizer` do that.
- **Audit and backups are process-wide.** `audit_events` rows are
  tenant-tagged but are read by platform tooling; backups and replicas contain
  every tenant's rows. Ban markers are keyed, but other retained data (for
  example audit metadata) is only as protected as the database and its
  credentials.
- **Deletion is logical first.** Erasure removes or anonymizes rows and blobs
  through the privacy erasers, but backups and replicas are not rewritten.

## Credential storage

Passwords are Argon2id PHC strings (`$argon2id$v=19$m=19456,t=2,p=1$...`,
OWASP Password Storage), normalized with NFKC and capped at 64 Unicode code
points (runes), with no bcrypt byte ceiling. Verification is constant-time; a successful sign-in
rehashes a hash written under older cost parameters (conditional on the hash
that verified, so a concurrent password change wins), and unknown-email logins
spend one comparison against a dummy hash with the same parameters. Ten MFA
recovery codes are 15 random bytes (120 bits, above the NIST SP 800-63B
112-bit lookup-secret minimum), displayed as six groups of four and stored as
SHA-256 hashes.

## Key management

Every secret is configuration, not a database value. `<NAME>_FILE` is accepted
for the secret settings listed in
[Operations](operations.md#11-configuration-reference); setting both forms is
an error. Every application key is base64-encoded 32 random bytes
(`openssl rand -base64 32`), validated after decoding; there are no built-in
sample keys. Each active key must be distinct (one key per purpose, NIST
SP 800-57; no `JWT_SECRET` fallback).

| Secret | Use | Rotation |
|---|---|---|
| `JWT_SECRET` (+ `JWT_PREVIOUS_SECRETS`) | HS256 access tokens pinned by `pkg/token`; `exp` is required and the header must be `typ: at+jwt` (RFC 9068); no email claim. Tokens carry a `kid` fingerprint of the signing secret. | Add the new secret as previous, roll out, promote it to active, keep the old one until its tokens expire (`JWT_ACCESS_TTL`, default 15m). `token.Keyring` picks the key by `kid`; an unknown `kid` is rejected. |
| `DATA_ENCRYPTION_KEY` (+ `DATA_ENCRYPTION_PREVIOUS_KEYS`) | AES-256-GCM sealing of email/password-reset job payloads and MFA secrets/challenges (`jobs.PayloadCodec`, HKDF-SHA256 with a purpose-specific info string). The same key set, under HKDF info `fgb/ban-email/v1`, is the HMAC-SHA256 key for banned-address markers. | Two-phase: previous on every replica, then active. Sealed jobs keep their `kid`; open accepts every configured key. See [Operations](operations.md#key-rotation). |
| `WEBHOOK_SECRET_ENCRYPTION_KEY` (+ previous keys) | Seal stored webhook endpoint/source secrets and delivery snapshots (v3 envelope; AAD binds purpose, tenant, and row id). | Re-seal by rotating each endpoint/source secret; old keys may be dropped once no snapshot uses them. |
| `OAUTH_STATE_KEY` | HMAC-SHA256 (after HKDF) for the OAuth state and PKCE cookie, so rotating `JWT_SECRET` does not invalidate in-flight Google sign-ins. | Safe to rotate when no sign-in is in flight (5-minute window). |
| Ban marker pepper (derived, not separate) | `memberships.banned_email_hash` is HMAC-SHA256 under the derived key; `banned_email_hash_kid` names it. A lookup tries the active and every previous key. | Removing a previous key makes the bans it wrote unmatchable — after erasure there is no address to re-hash. `cmd/api` startup logs and counts markers whose key is not configured. |
| `SMTP_PASSWORD`, `GOOGLE_CLIENT_SECRET`, `ADMIN_PASSWORD`, `METRICS_AUTH_TOKEN`, `STORAGE_S3_SECRET_KEY`, `REDIS_URL`, DB passwords | Transport/provider credentials, read from env or a mounted file. | Rotate at the provider and rollout; they are not stored in the database. |

Data-encryption keys are never used as HMAC keys directly: HKDF-SHA256 with a
purpose-specific `info` derives each key, so a ciphertext from one purpose
cannot be used in another. Key ids are one-way fingerprints, safe to log. The
file store is not a KMS; production operators are expected to mount these from
a secret manager.

## Supply chain

- Actions in every workflow are pinned to full commit SHAs with a trailing
  version comment. CI downloads `actionlint`, `kustomize`, `kubeconform`, and
  other tools only after checksum verification (see `CONTRIBUTING.md` and the
  workflow steps).
- The release builds the multi-arch image, pushes the exact digest, scans each
  platform digest with Trivy (CRITICAL/HIGH with fixes fail), and only then
  signs the **index digest** keylessly with cosign (OIDC identity =
  `release.yml` on `refs/tags/v<semver>`). Provenance and SBOM attestations are
  attached by the build.
- `release.yml` then runs `cosign verify` against that digest with the same
  OIDC issuer and the shared certificate-identity regexp that `deploy.yml`
  uses for staging (production narrows it to stable `v<semver>` tags; a stable
  release satisfies both), so a signature the deploy would refuse fails the
  release instead of production. The two workflows cannot share environment,
  so the issuer and pattern are duplicated at workflow level with a comment;
  keep them byte-identical.
- `deploy.yml` resolves the requested tag to its digest, verifies the digest
  (never a mutable tag) with that identity, and rolls out
  `image@sha256:…`. A Release-triggered deploy checks out the release's
  `head_sha`; pre-release tags never auto-deploy to production.
- The image is pushed before it is scanned or signed, so a failed scan leaves
  an **unsigned** image that the deploy rejects. Nothing in the release path
  can turn a failed signature check into a successful rollout.

## Reporting and review

Security-relevant changes (RLS, roles, keys, signing, erasure, ban handling)
should update this document and the arch tests in the same change. The
invariants are enforced by `test/arch` and `cmd/migrate`, not by this prose; if
the code and this document disagree, the code and tests win and this document
must be corrected.
