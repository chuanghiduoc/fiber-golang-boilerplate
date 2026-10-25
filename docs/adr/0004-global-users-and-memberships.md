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
