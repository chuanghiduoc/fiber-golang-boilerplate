# 0004. Global users with tenant memberships

- Status: Accepted
- Date: 2026-10-25

## Context

`users.tenant_id NOT NULL` makes every account belong to exactly one tenant;
a person in two tenants needs two accounts and two passwords. There are no
invitations. See the [AWS SaaS Lens](https://docs.aws.amazon.com/wellarchitected/latest/saas-lens/saas-lens.html)
for the user/tenant model.

## Decision

Users become global identities. A `memberships (tenant_id, user_id, status)`
table links users to tenants (N:N); `invitations` (hashed single-use token,
expiry, role) add members. Credentials and external identities move out of
`users` into `user_passwords` and `user_identities (provider, subject)`.
Sessions are first-class (`sessions` with a refresh-token family for reuse
detection per RFC 9700). The tenant of a request is selected explicitly
(token claim / header) and must match an active membership.

## Consequences

- Done before any production deployment, so the baseline schema changes in
  place (no data migration needed).
- Every tenant-scoped query keeps its `tenant_id` predicate; membership is
  checked at authentication time.

## Implementation notes (P2b-1)

- Schema lives in the `identity` section of `migrations/20260101000000_platform_init_schema.up.sql`; see
  [Data Model](../data-model.md#tenants-identity-and-credentials).
- `memberships.status` is `active | suspended | removed`; there is no
  `invited` state because a pending invite is an `invitations` row (it can
  target an address with no account). `memberships.role` was transitional and
  is removed by P2b-2; roles now live in `membership_roles` (ADR 0003).
- A session is the refresh-token family, so there is no separate family id;
  reuse revokes the session (`revoked_reason = 'refresh_reuse'`). Access
  tokens carry `sid`; tokens without it are rejected.
- Password-reset and email-verification tokens are per user, because the
  password is a global credential.
- Registering an address that already has an account is `409
  user.email_taken`; joining a second tenant requires an invitation, and
  Google sign-in never adds a membership to an existing user.
- Other modules reference `(tenant_id, user_id) → memberships ON DELETE
  RESTRICT`.
- P2b-2 implements the RBAC tables and platform-admin tenant endpoints
  (ADR 0003). Deferred: RLS (ADR 0007), MFA (ADR 0010).
