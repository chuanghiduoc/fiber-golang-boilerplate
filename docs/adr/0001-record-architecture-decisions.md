# 0001. Record architecture decisions as ADRs

- Status: Accepted
- Date: 2026-10-25

## Context

Design decisions (authorization model, tenancy, migration layout, API and
webhook formats) were previously captured only in long prose documents, which
drifted from the code across review rounds. Reviewers had no single place to
see what was decided, why, and what was rejected.

## Decision

Record every significant, hard-to-reverse decision as a short MADR file under
`docs/adr/`. Accepted ADRs are immutable; a later ADR supersedes an earlier one.
Code is the source of truth for behavior; ADRs record intent and trade-offs.

## Consequences

- Each architectural PR links its ADR.
- `docs/adr/` is owned in `.github/CODEOWNERS`.
- Long-form docs (`docs/*.md`) describe how the system works today; ADRs
  describe why.
