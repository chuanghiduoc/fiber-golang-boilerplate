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

`internal/bootstrap` attaches the webhooks module's dispatcher to identity and
files and `cmd/api/main.go` installs it in `app.Deps.Webhooks`; identity
registration and file upload/delete emit `user.registered`, `file.uploaded`,
and `file.deleted` through it. Modules can also inject the contract directly. A
nil dispatcher is a no-op, so tests and minimal deployments do not need the
webhooks module.

### Transactional dispatch (outbox)

`Dispatch` runs after the producer's commit, so a crash between the two could
lose (or, on a retry, duplicate) the event. Producers that own a transaction
use the webhooks dispatcher's `DispatchTx(ctx, tx pgx.Tx, events...)` instead.
It is not on the stdlib-only contract (it takes a `pgx.Tx`); producers declare
a narrow `WebhookTxDispatcher` interface and type-assert the injected
dispatcher, mirroring `jobs.EnqueueTx`/`events.PublishTx`.

`DispatchTx` fixes the event ID (assigning `evt_...` if empty) and enqueues one
`webhook.dispatch` job per event in the caller's transaction, deduplicated by
the idempotency key `webhook.dispatch:<eventId>`. The event and the business
change therefore commit or roll back together; an enqueue failure fails the
producer's mutation (fail-closed, like the email outbox). The job then runs the
normal fan-out with that stable ID; because deliveries are unique per
`(tenant, endpoint, event)`, a retried job only creates the deliveries still
missing. Endpoints are matched when the job runs, not at commit time.

Identity registration (when `TxManager` is wired) and file upload/delete (when
`filesvc.AttachTxManager` is wired, as in `files.Build`) use this path;
without it they fall back to the best-effort post-commit `Dispatch`.

### Endpoint model

| Field | Meaning |
|---|---|
| `url` | Absolute `http(s)` URL. The URL is snapshotted per delivery. |
| `eventTypes` | Patterns: exact (`file.uploaded`), prefix (`file.*`), or `*`. Empty means `*`. |
| `status` | `active` or `disabled`. Disabled endpoints are not matched, and their pending deliveries are not retried (see below). |
| `maxAttempts` | 1-50 delivery attempts (default `WEBHOOK_MAX_ATTEMPTS`). |
| `timeoutMs` | Per-attempt HTTP timeout, 100-120000 ms (default request timeout). Snapshotted per delivery; it can only shorten the global `WEBHOOK_REQUEST_TIMEOUT_SECS`. |
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
   endpoint URL, secret ciphertext, max attempts, and timeout.
5. Returns the number of newly created deliveries. `(tenant_id, endpoint_id,
   event_id)` is unique, so re-dispatching the same event is a no-op.

The worker then signs and sends one attempt per job. Retries are new jobs
scheduled at the delivery's next attempt time; the platform job is not used for
webhook-level backoff.

A delivery is only claimed while its endpoint is `active` and not deleted.
Once the request has been sent, the attempt row and the outcome are written
with a detached, bounded context, so a worker shutdown cannot lose a sent
attempt. When a pending delivery can never be claimed again — its endpoint was
disabled/deleted, or every attempt was consumed but the final bookkeeping write
failed — the next job for it marks it `dead` (`last_error` is `endpoint is
disabled or deleted` or `delivery attempts exhausted`) instead of leaving it
`pending` forever. A live claim lease is never touched.

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
<X-Webhook-Id>.<X-Webhook-Timestamp>.<X-Webhook-Event>.<raw-request-body>
```

> **Breaking change for receivers.** Earlier builds signed only
> `<timestamp>.<raw-body>`. The prefix stays `v1=` (still HMAC-SHA256), but a
> receiver verifying the old input will now reject every request as an invalid
> signature. Update receivers to the new input before deploying this build, or
> temporarily accept both inputs during the rollout. Binding the event ID and
> type closes a replay gap: previously an attacker who captured a request could
> resend it within the timestamp window under a fresh `X-Webhook-Id` (bypassing
> ID-based deduplication) or a different `X-Webhook-Event`.

Use the raw header values exactly as received (an absent `X-Webhook-Event`
signs as the empty string). Verify with `webhook.Sign`/`webhook.VerifyRequest`
and a small timestamp tolerance (default 5 minutes); always compare in constant
time. Support multiple comma-separated signatures to allow a receiver-side key
transition.

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
`Retry-After` (seconds or HTTP date) can only lengthen that backoff, never
shorten it, so `Retry-After: 0` or a past date cannot trigger immediate
retries; it is capped at the same maximum.
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
- treats IPv6 transition addresses by what they reach: NAT64 `64:ff9b::/96`,
  6to4 `2002::/16`, IPv4-compatible `::/96`, and IPv4-translated
  `::ffff:0:0:0/96` are re-checked against their embedded IPv4 address, while
  local-use NAT64 `64:ff9b:1::/48`, Teredo `2001::/32`, and site-local
  `fec0::/10` are blocked outright;
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
authenticates the request. An unknown or disabled source, a soft-deleted one,
or a source whose tenant is suspended returns `404`.

Required headers use the same scheme as outbound:

- `X-Webhook-Id`: external event ID (1-255 printable characters). Required and
  used for deduplication.
- `X-Webhook-Timestamp`: Unix seconds. Required.
- `X-Webhook-Signature`: `v1=<hex>`. Required.
- `X-Webhook-Event`: optional event type (default `inbound`).

The raw body must be valid JSON and at most 1 MiB. The signature covers
`<X-Webhook-Id>.<timestamp>.<X-Webhook-Event>.<raw-body>` (the same breaking
change as outbound; senders must sign the new input) and must be within
`WEBHOOK_TIMESTAMP_TOLERANCE_SECS`. Because the ID is signed, a captured
request cannot be replayed under a new ID to bypass deduplication. Invalid/missing signatures and stale
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

- Endpoint rotation locks the endpoint row, deactivates the old secret, and
  inserts the next version, so concurrent rotations serialize. Queued
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
Administration routes accept a JWT or an API key scoped to these permissions
(`app.Deps.Authenticate()`) and use `authz.Require`; regular users receive
`403`. The public receiver is the only unauthenticated route.

Administration mutations (endpoint/source create, update, delete,
rotate-secret, and delivery replay) are Tier 2: a required `audit.Guard`
writes a `started` intent row before the handler and completes it with the
outcome, so an unavailable audit store returns `503 system.audit_unavailable`
and the mutation does not run. The service then records the domain event as a
child (`parent_id`) of that intent: `webhook.endpoint.created|updated|deleted|
secret_rotated`, `webhook.delivery.replayed`, or
`webhook.inbound_source.created|updated|deleted|secret_rotated`. The public
receiver records `webhook.inbound.received` best-effort. Metadata contains
IDs, URL/filter/status, event type, and duplicate flag; it never contains
secrets or payloads. A domain-event write failure is logged and does not fail
the (already intent-audited) mutation.

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

Idempotency: routes that return a secret (endpoint/source create and both
`rotate-secret` routes) deliberately omit `Idempotency-Key` caching so the
plaintext secret never sits in the shared response cache. A retried rotation
rotates again and returns the newest secret; only the latest one is active.
Update, delete, and replay accept the header. The public receiver relies on
`(source, X-Webhook-Id)` deduplication and does not use the middleware.

## Tables and retention

- `webhook_endpoints` + `webhook_endpoint_secrets` (versioned, one active).
- `webhook_deliveries` (outbox + snapshot + state) and
  `webhook_delivery_attempts`.
- `webhook_inbound_sources` and `webhook_inbound_events` (unique per source and
  external event ID).

All tables carry `tenant_id` with tenant-leading indexes. There are no
cross-module foreign keys. Module-internal FKs are tenant-composite
(`(tenant_id, endpoint_id)`, `(tenant_id, source_id)`); secrets cascade with
their endpoint and attempts with their delivery, while deliveries and inbound
events are history and `RESTRICT` deletion of their endpoint/source. A retention loop runs hourly (first pass at startup) and deletes
`webhook_deliveries` in `succeeded`/`dead` state older than
`WEBHOOK_RETENTION_DAYS`, plus inbound events older than the same window.
Pending deliveries are never purged. Each purge deletes in batches of at most
1,000 rows (`FOR UPDATE SKIP LOCKED`) until a batch is short, bounded by a 30s
pass. Retention is cross-tenant, so its supporting indexes
(`idx_webhook_deliveries_terminal_updated_at`,
`idx_webhook_inbound_events_received_at`) are deliberately not tenant-leading;
`idx_webhook_deliveries_replayed_from_id` keeps the `ON DELETE SET NULL` on
replays from scanning the table per deleted row.

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

-- Outstanding fan-out and delivery jobs
SELECT id, tenant_id, kind, status, attempts, run_at, last_error
FROM jobs
WHERE kind IN ('webhook.dispatch', 'webhook.delivery') AND status IN ('queued', 'running')
ORDER BY run_at
LIMIT 50;
```

A stuck pending delivery with a future `next_attempt_at` usually means a worker
died after claiming it. The platform job lease retries the same job after
expiry; if no job remains, replay the delivery. Do not set terminal rows back
to pending in bulk: payloads and secrets are retained, but ordering, attempt
budgets, and receiver dedupe expectations still apply.
