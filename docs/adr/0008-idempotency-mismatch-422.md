# 0008. Idempotency key reused with a different body returns 422

- Status: Accepted
- Date: 2026-10-25

## Context

Reusing an `Idempotency-Key` with a different request body returned 409, the
same status as a duplicate still in progress, so clients could not tell a
programming error from a retry race. The IETF draft
[draft-ietf-httpapi-idempotency-key-header](https://datatracker.ietf.org/doc/draft-ietf-httpapi-idempotency-key-header/)
uses 422 for a mismatched payload and 409 for a concurrent duplicate.

## Decision

Return `422` (code `idempotency.key_mismatch`) for a body mismatch; keep `409`
for an in-progress duplicate.

## Consequences

Breaking change for clients that matched 409 on mismatch; documented in the
CHANGELOG. The draft expired without becoming an RFC, so this follows the
draft's convention, not a ratified standard.
