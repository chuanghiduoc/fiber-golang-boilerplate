# Data Model

The schema lives in one baseline, `migrations/000001_init_schema.up.sql`,
applied only by `cmd/migrate`. Every table has exactly one owner; modules read
other modules' data through contracts, never through SQL (enforced by
`test/arch`). See [Architecture](architecture.md#schema-ownership) for the
migration policy.

## Ownership at a glance

| Owner | Tables |
|---|---|
| `identity` | `tenants`, `users`, `refresh_tokens`, `password_reset_tokens`, `email_verification_tokens` |
| `apikeys` | `api_keys` |
| `files` | `files` |
| `notifications` | `notifications`, `notification_preferences` |
| `features` | `feature_flags` (global), `feature_flag_targets` |
| `webhooks` | `webhook_endpoints`, `webhook_endpoint_secrets`, `webhook_deliveries`, `webhook_delivery_attempts`, `webhook_inbound_sources`, `webhook_inbound_events` |
| platform | `jobs`, `audit_events`, `scheduler_leases`, `scheduler_runs` |

Conventions that apply to every diagram below:

- Every tenant-owned table has `tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES
  tenants(id)`; the diagrams draw that edge only once (first diagram) to keep
  them readable. Tenant `1` (`default`) is seeded by the baseline.
- Child rows reference their parent through a **composite** `(tenant_id, id)`
  foreign key (for example `(tenant_id, user_id) → users(tenant_id, id)`), so a
  row can never point at a parent in another tenant.
- Primary keys are `BIGINT GENERATED ALWAYS AS IDENTITY`, except
  `feature_flags.flag_key` and `scheduler_leases.name`. Timestamps are
  `TIMESTAMPTZ`.
- Delete actions: `CASCADE` only for rows fully owned by their parent (auth
  tokens, endpoint secrets, delivery attempts); `RESTRICT` for business and
  history rows; `SET NULL` for `webhook_deliveries.replayed_from_id`.

## Tenants, identity, and credentials

```mermaid
erDiagram
    tenants ||--o{ users : "tenant_id"
    users ||--o{ refresh_tokens : "CASCADE"
    users ||--o| password_reset_tokens : "one per user, CASCADE"
    users ||--o| email_verification_tokens : "one per user, CASCADE"
    users ||--o{ api_keys : "RESTRICT"

    tenants {
        bigint id PK
        varchar slug UK "[a-z0-9-]{1,63}"
        varchar name
        varchar status "active | suspended"
    }
    users {
        bigint id PK
        bigint tenant_id FK
        varchar email "unique per tenant (lower, active rows)"
        varchar google_id "unique per tenant (active rows)"
        varchar role "user | admin"
        varchar auth_provider "local | google"
        int token_version "bumped to revoke access tokens"
        timestamptz banned_at
        timestamptz credentials_revoked_at
        timestamptz deleted_at "soft delete"
    }
    refresh_tokens {
        bigint id PK
        bigint tenant_id FK
        bigint user_id FK
        varchar token UK
        int token_version
        timestamptz expires_at
    }
    password_reset_tokens {
        bigint id PK
        bigint tenant_id FK
        bigint user_id FK
        varchar token UK
        timestamptz expires_at
    }
    email_verification_tokens {
        bigint id PK
        bigint tenant_id FK
        bigint user_id FK
        varchar email
        varchar token UK
        timestamptz expires_at
    }
    api_keys {
        bigint id PK
        bigint tenant_id FK
        bigint user_id FK
        varchar prefix
        varchar key_hash UK "SHA-256 only"
        jsonb scopes "permission array"
        timestamptz expires_at
        timestamptz revoked_at
        timestamptz rotation_grace_until
        timestamptz last_used_at
    }
```

- A trigger refuses to remove or demote the last admin **of a tenant**.
- `api_keys.key_hash` is the only global (pre-tenant) lookup: the request
  tenant cannot be trusted until the key is verified.

## Files, notifications, and feature flags

```mermaid
erDiagram
    users ||--o{ files : "RESTRICT"
    users ||--o{ notifications : "RESTRICT"
    users ||--o{ notification_preferences : "RESTRICT"
    feature_flags ||--o{ feature_flag_targets : "flag_key, RESTRICT"
    users |o--o{ feature_flag_targets : "user target (nullable), RESTRICT"

    files {
        bigint id PK
        bigint tenant_id FK
        bigint user_id FK
        varchar storage_path UK "blob key"
        varchar mime_type
        bigint size
        timestamptz deleted_at "soft delete / trash"
    }
    notifications {
        bigint id PK
        bigint tenant_id FK
        bigint user_id FK
        varchar type
        jsonb data
        timestamptz read_at
    }
    notification_preferences {
        bigint id PK
        bigint tenant_id FK
        bigint user_id FK
        varchar type
        varchar channel "in_app | email"
        bool enabled
    }
    feature_flags {
        varchar flag_key PK "global, no tenant_id"
        bool enabled "NULL = no database default"
        bool kill_switch
    }
    feature_flag_targets {
        bigint id PK
        varchar flag_key FK
        bigint tenant_id FK
        bigint user_id FK "NULL = tenant-wide target"
        bool enabled
    }
```

| Table | Key constraint / index | Why |
|---|---|---|
| `files` | `storage_path` unique | Authoritative blob key (`<tenantID>/<userID>/<uuid>.<ext>` for new uploads). |
| `notifications` | feed index `(tenant_id, user_id, created_at DESC, id DESC)` + partial unread variant | Cursor feed and unread badge. |
| `notification_preferences` | unique `(tenant_id, user_id, type, channel)` | Idempotent upsert; a missing row means enabled. |
| `feature_flag_targets` | unique `(flag_key, tenant_id)` where `user_id IS NULL`; unique `(flag_key, tenant_id, user_id)` otherwise | One tenant target and one target per user. |

## Webhooks

```mermaid
erDiagram
    webhook_endpoints ||--|{ webhook_endpoint_secrets : "versions, CASCADE"
    webhook_endpoints ||--o{ webhook_deliveries : "RESTRICT"
    webhook_deliveries ||--o{ webhook_delivery_attempts : "CASCADE"
    webhook_deliveries |o--o{ webhook_deliveries : "replayed_from_id, SET NULL"
    webhook_inbound_sources ||--o{ webhook_inbound_events : "RESTRICT"

    webhook_endpoints {
        bigint id PK
        bigint tenant_id FK
        varchar url
        text_array event_types "default *"
        varchar status "active | disabled"
        int max_attempts "1..50"
        int timeout_ms "100..120000"
        timestamptz deleted_at
    }
    webhook_endpoint_secrets {
        bigint id PK
        bigint tenant_id FK
        bigint endpoint_id FK
        int version "unique per endpoint"
        text secret_ciphertext "AES-256-GCM"
        bool active
    }
    webhook_deliveries {
        bigint id PK
        bigint tenant_id FK
        bigint endpoint_id FK
        varchar event_id "unique per endpoint"
        varchar url "snapshot"
        jsonb payload "snapshot"
        text secret_ciphertext "snapshot"
        varchar status "pending | succeeded | dead"
        int attempts
        timestamptz next_attempt_at
        bigint replayed_from_id FK
    }
    webhook_delivery_attempts {
        bigint id PK
        bigint tenant_id FK
        bigint delivery_id FK
        int attempt "unique per delivery"
        int status_code
        int duration_ms
    }
    webhook_inbound_sources {
        bigint id PK
        bigint tenant_id FK
        varchar slug "unique per tenant (active)"
        text secret_ciphertext
        varchar status "active | disabled"
    }
    webhook_inbound_events {
        bigint id PK
        bigint tenant_id FK
        bigint source_id FK
        varchar external_event_id "X-Webhook-Id"
        jsonb payload
    }
```

- `(tenant_id, endpoint_id, event_id)` is unique, so re-dispatching the same
  domain event never creates a second delivery.
- `(tenant_id, source_id, external_event_id)` is unique: the inbound dedupe key.
- Delivery `url`/`payload`/`secret_ciphertext` are snapshots; editing or
  rotating an endpoint cannot change a queued request. Deliveries use
  `RESTRICT` because endpoints are soft-deleted and the delivery log is history.

## Platform tables

```mermaid
erDiagram
    tenants ||--o{ jobs : "tenant_id"
    tenants ||--o{ audit_events : "tenant_id"
    audit_events |o..o{ audit_events : "parent_id (logical, no FK)"
    scheduler_runs }o..o| jobs : "job_id (logical, no FK)"

    jobs {
        bigint id PK
        bigint tenant_id FK
        text kind
        jsonb payload "sealed for email jobs"
        text status "queued | running | succeeded | failed | dead"
        int attempts "fences bookkeeping writes"
        int max_attempts
        text idempotency_key "nullable, unique per tenant"
        timestamptz run_at
        text locked_by "owning worker"
        timestamptz lease_expires_at "required while running"
        text trace_parent "W3C span link"
    }
    audit_events {
        bigint id PK
        bigint tenant_id FK
        bigint parent_id
        text actor_type
        text action
        text resource
        text source "http | service | job | system"
        text outcome "started | succeeded | failed | denied | unknown"
        timestamptz completed_at
    }
    scheduler_leases {
        text name PK "one leader row"
        text holder
        timestamptz expires_at
    }
    scheduler_runs {
        bigint id PK
        text schedule
        timestamptz scheduled_for "unique with schedule"
        text status "enqueued | skipped | failed"
        bigint job_id
    }
```

- `jobs` is the durable queue and outbox. The partial unique
  `(tenant_id, idempotency_key)` index backs `EnqueueOnce`/`EnqueueOnceTx`.
  Claim and purge are global passes by design. `lease_expires_at` is
  `now()` + the owning worker's lease, written on claim and every heartbeat;
  the `jobs_running_has_lease` `CHECK` requires it on `running` rows, and
  lease recovery reclaims only rows where it is past (partial index
  `idx_jobs_running_lease_expires_at`).
- `audit_events` is append-only: triggers reject `DELETE`/`TRUNCATE` and limit
  updates to completing a `started` intent.
- `scheduler_leases` and `scheduler_runs` are deliberately tenant-independent
  with no foreign keys; the unique `(schedule, scheduled_for)` ledger is the
  exactly-once boundary across leader changes.
