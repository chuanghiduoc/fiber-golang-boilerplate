# Webhooks

Tenant-scoped outbound webhooks, durable delivery, and a public
HMAC-authenticated inbound receiver. The module lives in
`internal/modules/webhooks`; events are published through the stdlib-only
contract in `internal/contracts/webhooks`; signing and SSRF-safe HTTP live in
`pkg/webhook`.

## Outbound

### Event contract

Business modules emit `webhooks.Event` through `webhooks.Dispatcher` without
importing the webhooks module:

```go
type Event struct {
    ID         string
    Type       string
    TenantID   int64
    OccurredAt time.Time
    Data       json.RawMessage
}

type Dispatcher interface {
    Dispatch(ctx context.Context, events ...Event) (int, error)
}
```

- `Type` is a concrete lowercase dotted name (`file.uploaded`,
  `user.registered`). Patterns (`file.*`, `*`) are endpoint filters, not event
  types.
- Supply a stable `ID` when the producer may retry. The dispatcher assigns an
  `evt_...` ID when empty, but a retry that generates a new ID is a new logical
  event and cannot be deduplicated.
- A zero `TenantID` resolves from the context (`tenant.EffectiveID`).
- `Data` defaults to `{}`; it must be valid JSON.

`cmd/api/main.go` installs the dispatcher in `app.Deps.Webhooks`; identity
registration and file upload/delete emit `user.registered`, `file.uploaded`,
and `file.deleted` through it. Modules can also inject the contract directly. A
nil dispatcher is a no-op, so tests and minimal deployments do not need the
webhooks module.

### Endpoint model

| Field | Meaning |
|---|---|
| `url` | Absolute `http(s)` URL. The URL is snapshotted per delivery. |
| `eventTypes` | Patterns: exact (`file.uploaded`), prefix (`file.*`), or `*`. Empty means `*`. |
| `status` | `active` or `disabled`. Disabled endpoints are not matched. |
| `maxAttempts` | 1-50 delivery attempts (default `WEBHOOK_MAX_ATTEMPTS`). |
| `timeoutMs` | Per-attempt HTTP timeout, 100-120000 ms (default request timeout). |
| `description` | Free-form label. |

Delete is soft: the row keeps its delivery history but is excluded from
matching and returns `404` for reads.

### Dispatch flow

For each event, the dispatcher:

1. Resolves/validates the tenant, event ID, type, timestamp, and JSON body.
2. Loads the tenant's active endpoints with their active sealed secret.
3. Filters by `eventTypes`.
4. Inserts a `webhook_deliveries` row and a `webhook.delivery` job in **one
   transaction**. The delivery snapshots the event payload, event timestamp,
   endpoint URL, secret ciphertext, and max attempts.
5. Returns the number of newly created deliveries. `(tenant_id, endpoint_id,
   event_id)` is unique, so re-dispatching the same event is a no-op.

The worker then signs and sends one attempt per job. Retries are new jobs
scheduled at the delivery's next attempt time; the platform job is not used for
webhook-level backoff.

### Signed request

Headers:

| Header | Value |
|---|---|
| `X-Webhook-Id` | Event ID (stable across delivery retries). |
| `X-Webhook-Delivery` | Delivery ID. **Deduplicate on this value**; delivery is at-least-once. |
| `X-Webhook-Event` | Event type. |
| `X-Webhook-Timestamp` | Unix seconds used in the signature. |
| `X-Webhook-Signature` | `v1=<lowercase hex HMAC-SHA256>`. |
| `X-Webhook-Attempt` | 1-based attempt number. |
| `User-Agent` | `fiber-golang-boilerplate-webhooks/1.0`. |

Body:

```json
{
  "id": "evt_...",
  "type": "file.uploaded",
  "createdAt": "2026-10-24T12:00:00Z",
  "data": {"fileId": 42}
}
```

Signature input is the exact bytes:

```
<unix-timestamp>.<raw-request-body>
```

Verify with `webhook.Sign`/`webhook.VerifyRequest` and a small timestamp
tolerance (default 5 minutes); always compare in constant time. Support
multiple comma-separated signatures to allow a receiver-side key transition.

### Retry classification

| Response | Outcome |
|---|---|
| `2xx` | Succeeded. |
| `3xx` | Dead. Redirects are never followed. |
| `4xx` | Dead, except `408`, `425`, `429`. |
| `408` / `425` / `429` | Retry while attempts remain. |
| `5xx` | Retry while attempts remain. |
| transport/DNS/timeout | Retry while attempts remain. |
| blocked/invalid target | Dead immediately. |

Backoff is `base * 2^(attempt-1)` capped at `WEBHOOK_RETRY_MAX_SECS`. A valid
`Retry-After` (seconds or HTTP date) wins and is capped at the same maximum.
When attempts are exhausted, or the failure is permanent, the delivery becomes
`dead`. Every attempt is recorded in `webhook_delivery_attempts` with status
code, duration, and a truncated error (never the response body).

Replay (`POST /webhooks/deliveries/:id/replay`) creates a **new** delivery using
the retained payload, the endpoint's current URL, and its current active secret.
The original row is untouched.

### SSRF safety

`pkg/webhook.Client`:

- accepts only absolute `http(s)` URLs with no userinfo;
- resolves the hostname itself, rejects private, loopback, link-local,
  multicast, unspecified, CGNAT, and reserved/documentation ranges;
- dials the vetted IP while preserving the hostname for `Host`/TLS SNI, which
  prevents DNS rebinding between check and connect;
- sets `Proxy: nil` so `HTTP_PROXY` cannot bypass validation;
- never follows redirects (a 3xx is a terminal dead delivery);
- applies connect, response-header, and overall timeouts and bounds the response
  body it reads.

`WEBHOOK_SSRF_ALLOWLIST` permits specific trusted private receivers (comma-
separated IPs, CIDRs, or exact hostnames). Leave it empty unless a receiver is
intentionally internal. Never use `0.0.0.0/0` or `::/0`.

## Inbound

`POST /api/v1/webhooks/inbound/{sourceID}` is public: no JWT and no
`X-Tenant-Slug`. The source ID selects the owning tenant and the source secret
authenticates the request. An unknown or disabled source, or a soft-deleted
one, returns `404`.

Required headers use the same scheme as outbound:

- `X-Webhook-Id`: external event ID (1-255 printable characters). Required and
  used for deduplication.
- `X-Webhook-Timestamp`: Unix seconds. Required.
- `X-Webhook-Signature`: `v1=<hex>`. Required.
- `X-Webhook-Event`: optional event type (default `inbound`).

The raw body must be valid JSON and at most 1 MiB. The signature covers
`<timestamp>.<raw-body>` and must be within
`WEBHOOK_TIMESTAMP_TOLERANCE_SECS`. Invalid/missing signatures and stale
timestamps return `401`; malformed bodies/IDs return `400`.

Responses:

- `202 Accepted` with `{id, tenantId, sourceId, eventId, eventType, receivedAt,
  duplicate: false}` for a new event.
- `200 OK` with `duplicate: true` and the original row for a repeated
  `(source, X-Webhook-Id)`. The body is stored once.
- The stored row keeps the signature and payload but does not echo them back.

The handler installs the source tenant in Fiber locals/context before returning,
so the generic audit middleware records `webhook.inbound.received` under the
source tenant, not the request's default tenant.

## Secrets and configuration

Endpoint secrets are 32 random bytes (`whsec_...`); inbound source secrets use
`whin_...`. Both are sealed with AES-256-GCM using HKDF-SHA256 domain separation
from `WEBHOOK_SECRET_ENCRYPTION_KEY`. In local/test, an empty dedicated key
falls back to `JWT_SECRET`; **production requires the dedicated key** (at least
32 characters). Only ciphertext is stored in `webhook_endpoint_secrets` and
`webhook_inbound_sources`. Plaintext is returned exactly once on create or
rotate.

Rotation:

- Endpoint rotation deactivates the old secret and inserts a new version. Queued
  deliveries keep the ciphertext snapshot they were created with, so rotation
  does not break in-flight work. Replays use the current active secret.
- Inbound rotation replaces the verification secret immediately; old signatures
  fail on the next request.
- Rotating `WEBHOOK_SECRET_ENCRYPTION_KEY` makes all stored endpoint/inbound
  secrets unreadable. Rotate every webhook secret after changing the key.

| Variable | Default | Purpose |
|---|---|---|
| `WEBHOOK_SECRET_ENCRYPTION_KEY` | empty (JWT fallback) | AES-256-GCM secret-encryption key; required in production. |
| `WEBHOOK_CONNECT_TIMEOUT_SECS` | `5` | TCP/TLS connect timeout. |
| `WEBHOOK_REQUEST_TIMEOUT_SECS` | `10` | Per-attempt timeout, max 120. |
| `WEBHOOK_RETRY_BASE_SECS` | `5` | First retry backoff. |
| `WEBHOOK_RETRY_MAX_SECS` | `3600` | Backoff/Retry-After cap, max 7 days. |
| `WEBHOOK_MAX_ATTEMPTS` | `8` | Default delivery attempt budget (1-50). |
| `WEBHOOK_RETENTION_DAYS` | `30` | Terminal delivery/inbound-event retention. |
| `WEBHOOK_TIMESTAMP_TOLERANCE_SECS` | `300` | Inbound signature clock skew. |
| `WEBHOOK_SSRF_ALLOWLIST` | empty | Trusted private IPs/CIDRs/hosts. |

## Permissions and audit

`webhooks:read` and `webhooks:write` are granted to the built-in `admin` role.
Every route uses `authz.Require`; regular users receive `403`. The public
receiver is the only unauthenticated route.

Domain audit actions include `webhook.endpoint.created|updated|deleted|
secret_rotated`, `webhook.delivery.replayed`,
`webhook.inbound_source.created|updated|deleted|secret_rotated`, and
`webhook.inbound.received`. Metadata contains IDs, URL/filter/status, event
type, and duplicate flag; it never contains secrets or payloads. Audit write
failures are logged and do not fail the mutation.

## HTTP API

| Method | Path | Permission |
|---|---|---|
| GET | `/api/v1/webhooks/endpoints` | `webhooks:read` |
| POST | `/api/v1/webhooks/endpoints` | `webhooks:write` |
| GET | `/api/v1/webhooks/endpoints/:id` | `webhooks:read` |
| PUT | `/api/v1/webhooks/endpoints/:id` | `webhooks:write` |
| DELETE | `/api/v1/webhooks/endpoints/:id` | `webhooks:write` |
| POST | `/api/v1/webhooks/endpoints/:id/rotate-secret` | `webhooks:write` |
| GET | `/api/v1/webhooks/deliveries` | `webhooks:read` |
| GET | `/api/v1/webhooks/deliveries/:id` | `webhooks:read` |
| GET | `/api/v1/webhooks/deliveries/:id/attempts` | `webhooks:read` |
| POST | `/api/v1/webhooks/deliveries/:id/replay` | `webhooks:write` |
| GET | `/api/v1/webhooks/inbound-sources` | `webhooks:read` |
| POST | `/api/v1/webhooks/inbound-sources` | `webhooks:write` |
| GET | `/api/v1/webhooks/inbound-sources/:id` | `webhooks:read` |
| PUT | `/api/v1/webhooks/inbound-sources/:id` | `webhooks:write` |
| DELETE | `/api/v1/webhooks/inbound-sources/:id` | `webhooks:write` |
| POST | `/api/v1/webhooks/inbound-sources/:id/rotate-secret` | `webhooks:write` |
| GET | `/api/v1/webhooks/inbound-sources/:id/events` | `webhooks:read` |
| POST | `/api/v1/webhooks/inbound/{id}` | Public, HMAC |

Idempotency: creates that return a secret deliberately omit `Idempotency-Key`
caching. Every other mutation accepts it; replaying a rotate request returns the
same secret instead of rotating again. The public receiver relies on
`(source, X-Webhook-Id)` deduplication and does not use the middleware.

## Tables and retention

- `webhook_endpoints` + `webhook_endpoint_secrets` (versioned, one active).
- `webhook_deliveries` (outbox + snapshot + state) and
  `webhook_delivery_attempts`.
- `webhook_inbound_sources` and `webhook_inbound_events` (unique per source and
  external event ID).

All tables carry `tenant_id` with tenant-leading indexes. There are no
cross-module foreign keys; module-internal FKs cascade delivery attempts and
source events. A retention loop runs hourly (first pass at startup) and deletes
`webhook_deliveries` in `succeeded`/`dead` state older than
`WEBHOOK_RETENTION_DAYS`, plus inbound events older than the same window.
Pending deliveries are never purged.

## Inspecting and operating

```sql
-- Backlog by tenant/status
SELECT tenant_id, status, count(*), min(next_attempt_at), max(updated_at)
FROM webhook_deliveries
GROUP BY tenant_id, status
ORDER BY count(*) DESC;

-- Dead deliveries and last failures
SELECT id, tenant_id, endpoint_id, event_type, attempts, max_attempts,
       last_status_code, last_error, updated_at
FROM webhook_deliveries
WHERE status = 'dead'
ORDER BY updated_at DESC
LIMIT 50;

-- Recent attempts for one delivery
SELECT attempt, status_code, duration_ms, error, created_at
FROM webhook_delivery_attempts
WHERE delivery_id = $1
ORDER BY attempt;

-- Outstanding delivery jobs
SELECT id, tenant_id, status, attempts, run_at, last_error
FROM jobs
WHERE kind = 'webhook.delivery' AND status IN ('queued', 'running')
ORDER BY run_at
LIMIT 50;
```

A stuck pending delivery with a future `next_attempt_at` usually means a worker
died after claiming it. The platform job lease retries the same job after
expiry; if no job remains, replay the delivery. Do not set terminal rows back
to pending in bulk: payloads and secrets are retained, but ordering, attempt
budgets, and receiver dedupe expectations still apply.
