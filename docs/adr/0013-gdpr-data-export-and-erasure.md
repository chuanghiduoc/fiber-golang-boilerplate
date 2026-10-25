# 0013. GDPR data export and erasure

- Status: Accepted
- Date: 2026-10-25

## Context

Users can request a copy of their personal data (GDPR Art. 15) and erasure of
it (Art. 17). Per-user data is spread across identity, files, notifications,
apikeys, and features; a module may not read or delete another module's rows
(`CLAUDE.md`, [ADR 0002](0002-module-layering.md)). Audit rows are append-only
and may not store PII, so the audit trail itself cannot be the erasure
mechanism.

## Options considered

1. **A central privacy module with SQL across every owner.** Simple fan-out,
   but it duplicates every module's schema and breaks one-owner-per-table.
2. **Per-module exporters/erasers behind a stdlib contract, orchestrated by
   identity** (chosen). Each module keeps its own rules and table ownership;
   identity owns the request table and the global account.
3. **An external DSR provider.** Operationally heavier and still needs the
   same per-module data access.

## Decision

- `internal/contracts/privacy` defines `Exporter` (one named JSON section for
  one user in one tenant), `Eraser` (idempotent delete/anonymize for one user
  in one tenant), and a startup `Registry`. Modules register through the
  optional `app.PrivacyProvider` hook. Webhooks registers for stored event
  history and deliveries whose envelope/payload names a user; endpoint
  configuration itself is tenant-wide and is not touched.
- Identity owns `data_exports` and both HTTP surfaces. A request creates a
  `queued` row and a `privacy.data_export` job in one transaction. The job runs
  every exporter, writes one JSON document to
  `<tenantID>/<userID>/exports/<uuid>.json`, marks the row `ready` (7-day
  expiry), and notifies the user. A partial unique index allows one active
  (`queued`/`running`) export per membership; a second request is 409.
- Erasure re-authenticates the caller (current password; the code base has no
  MFA yet) and enqueues one `privacy.erase` job per membership plus a
  retryable `privacy.erase.finalize` job. The per-tenant job re-checks the
  last-admin rule before any eraser and then runs every eraser; the finalizer
  anonymizes the global user (`erased+<id>@invalid`, empty name,
  `status = deleted`), deletes credentials, MFA factors and recovery codes,
  external identities, sessions, and
  tokens, and only succeeds once every membership is `removed`. It also
  upgrades an account deleted by the older self-service path, which left
  credentials and PII behind. The admin variant
  (`POST /admin/users/:id/erasure`, `admin:users:delete`) uses the same
  fan-out. Both erasure routes are JWT-only. The last active admin of any
  tenant is refused with 409 before anything is enqueued, and is re-checked
  at job time.
- Every job is at-least-once and idempotent. A double request is guarded by
  the partial unique index; erasure wins a race with an in-flight export by
  marking the export `expired` and by the job's conditional
  `running → ready` update, so a late writer deletes its own document. Export
  requests hold the membership `FOR SHARE`, and the finalizer sweeps any export
  that committed just before its membership was removed. Expired exports,
  failed builds, and stale queued/running rows older than one day are removed
  blob-first, row-second by the hourly `privacy.exports.purge` schedule.
- Audit rows keep only IDs and outcomes (no email, name, or document content).
  They are the append-only record that a request happened; personal data is
  deleted or anonymized under retention rules instead.

## Consequences

- Five additive paths (six operations); no existing response or route changes.
- New identity-owned `data_exports` table, indexes, and `updated_at` trigger.
- Export documents include file blobs base64-encoded, so a very large account
  produces a large JSON document and the job may use substantial memory; the
  job timeout and 7-day retention bound the cost.
- `audit_events` remains append-only and PII-free. `jobs` payloads carry only
  IDs. RLS and per-tenant quotas remain future work (ADR 0007).
