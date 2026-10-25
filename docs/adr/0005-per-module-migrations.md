# 0005. Per-module baseline migrations

- Status: Accepted
- Date: 2026-10-25

## Context

A single `000001_init_schema` baseline owned all tables. With 50+ modules it
becomes the biggest merge-conflict hotspot, and it hides table ownership.

## Decision

Split the baseline into ordered per-owner baseline files (platform first, then
modules in dependency order) using the existing `<UTC timestamp>_<owner>_<purpose>`
naming. Each module owns its migration files; cross-module foreign keys may
only target `tenants` (and the identity tables during the transition).

## Consequences

- `test/arch` validates naming, ownership, and up/down pairs per owner.
- `cmd/migrate` keeps verifying the resulting schema.
- No deployed database exists yet, so renumbering is safe now and only now.
