# 0012. Single init migration with owner sections

- Status: Accepted
- Date: 2026-10-25
- Supersedes: [0005](0005-per-module-migrations.md) (file layout only)

## Context

ADR 0005 split the fresh-install schema into ten per-owner baseline files.
For a boilerplate that is harder to read than one schema file, and nothing
has been deployed, so the split buys no upgrade safety yet.

## Decision

Fresh installs apply one pair,
`migrations/20260101000000_platform_init_schema.{up,down}.sql`. Each section
starts with `-- owner: <module|platform>` and may only create its owner's
tables. It is edited in place until the first release.

Every later change is a new migration named
`<YYYYMMDDHHMMSS UTC>_<owner>_<purpose>.{up,down}.sql`
(`make migrate-create` or `make gen`). Ownership rules from 0005 stay:
cross-module foreign keys target only `tenants` and `memberships`.

## Consequences

- `test/arch` checks timestamp names, up/down pairs, known owners, and table
  ownership per file and per `-- owner:` section.
- A legacy database is recorded with `force 20260101000000` after manual DDL.
- After the first release the init is frozen; only new migrations are added.
