# Operations Runbook

Runbook for the deployed API: health, metrics, logs, jobs, webhooks,
idempotency, migrations, Kubernetes, and incident response. It describes the
behavior in this repository; replace every `<placeholder>` and adapt thresholds
to your service levels.

Related documents:

- [Kubernetes deployment](../deploy/k8s/README.md) — manifests, staging/
  production overlays, GitHub Actions CD, migration order, rollback.
- [Architecture](architecture.md) — modules, dependency rules, data ownership.
- [Webhooks](webhooks.md) — outbound signing/retries/SSRF, inbound receiver,
  secrets, and configuration.
- [Module Template](module-template.md) — adding a module, routes, permissions,
  tables, and contracts.

Never put real secrets in this file, a ticket, a log line, or an error message.

## 1. Health and readiness

| Endpoint | Purpose | Behavior |
| --- | --- | --- |
| `GET /healthz` | Liveness | Always `200` with `{"status":"up"}` while the process runs. Does not check dependencies. |
| `GET /readyz` | Readiness | Checks PostgreSQL and cache concurrently (2s each). `503` with `status:"down"` only when PostgreSQL is down; a cache outage or a saturated DB pool is `200` with `status:"degraded"`. |
| `GET /health` | Readiness alias | Same handler and response as `/readyz`. |
| `GET /metrics` | Prometheus | Bearer token required when `METRICS_AUTH_TOKEN` is set. Production config requires a token of at least 16 characters. |
| `GET /swagger` | API docs | Non-production only. |

Readiness details contain only `database` (`up`, `busy`, or `down`) and
`cache` (`up` or `down`). Pool sizes are not exposed on the public probe:

| `database` | `cache` | `status` | HTTP |
| --- | --- | --- | --- |
| `up` | `up` | `up` | `200` |
| `up` | `down` | `degraded` | `200` |
| `busy` | any | `degraded` | `200` |
| `down` | any | `down` | `503` |

`busy` means the ping timed out while every pool connection was checked out
(`AcquiredConns >= MaxConns`). Those connections are serving requests against a
reachable database, so the pod stays in rotation; removing every saturated pod
at once would cascade the load. If the database is really gone, in-flight
queries fail and release their connections, and the next probe reports `down`.

```bash
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
curl -fsS -H "Authorization: Bearer $METRICS_AUTH_TOKEN" \
  http://127.0.0.1:8080/metrics | head
```

In Kubernetes:

```bash
kubectl -n <namespace> port-forward svc/<service> 8080:8080
kubectl -n <namespace> get pods -w
kubectl -n <namespace> describe pod <pod>
```

Probe behavior:

- The startup probe uses `/healthz`, but it only covers slow-but-reachable
  startup. The API exits during startup if PostgreSQL, storage (including the
  S3 bucket check), or cache initialization fails, so the probe cannot mask a
  dependency outage; the container exits and restarts instead.
- Readiness uses `/readyz`; a failed readiness check removes the pod from the
  Service but does not restart it.
- Liveness uses `/healthz`; do not point liveness at `/readyz` or a Redis blip
  will restart every pod.
- Readiness does **not** check SMTP, S3 object operations, the job queue, or
  whether migrations ran. The S3 bucket check happens at startup; after
  startup, object reads/writes fail on use. SMTP failures surface through
  queued jobs, migrations are a separate Job, and queue health is visible in
  job metrics and the `jobs` table. Do not assume a green `/readyz` means email
  or uploads are working.

Trade-off: a Redis outage keeps every replica ready (`degraded`) because rate
limiting and idempotency fail open and sessions fall back to PostgreSQL. The
API keeps serving with weaker abuse protection and possible duplicate
mutations; alert on `status:"degraded"` and `rate_limit_errors_total` instead of
relying on readiness to page.

## 2. Metrics

`/metrics` is the Prometheus endpoint. Set `METRICS_AUTH_TOKEN` in production
and configure the scraper to send:

```yaml
authorization:
  credentials: <metrics-token>
```

Application metrics:

| Metric | Labels | Meaning |
| --- | --- | --- |
| `http_requests_total` | `method`, `path`, `status` | Completed HTTP requests. `path` is the Fiber route template, not the raw URL. |
| `http_request_duration_seconds` | `method`, `path` | HTTP latency histogram (default Prometheus buckets). |
| `jobs_claimed_total` | `kind` | Jobs claimed by workers. |
| `jobs_succeeded_total` | `kind` | Successful outcomes recorded in the database. |
| `jobs_retried_total` | `kind` | Failed jobs requeued for another attempt. |
| `jobs_dead_total` | `kind` | Handler failures that exhausted attempts. Lease-expiry dead letters are visible in the table but not counted here. |
| `jobs_queue_lag_seconds` | `kind` | Histogram of seconds between a job becoming due (`run_at`) and its claim, computed by the claim query (database clock; no extra query). Only claimed jobs are observed, so a fully stalled queue shows up as missing observations; alert on `jobs_claimed_total` too. |
| `jobs_claim_errors_total` | — | Failed claim attempts; the worker retries with backoff capped at 30s. |
| `jobs_purged_total` | — | Terminal job rows deleted by retention. |
| `audit_write_failures_total` | — | Best-effort audit events that could not be recorded. |
| `audit_intent_failures_total` | — | Pre-write intent rows that could not be recorded (required guards returned 503). |
| `audit_completion_failures_total` | — | Intent rows that could not be completed; inspect stale `started` rows. |
| `audit_tx_missing_total` | — | Tier 1 audit writes attempted without a transaction (misconfiguration). |
| `audit_domain_missing_total` | — | Guarded successes without the required domain event. |
| `feature_flag_evaluations_total` | `key`, `source`, `enabled` | Feature evaluations by precedence source. |
| `feature_flag_snapshot_loads_total` | `result` | Feature snapshot loads (`success`/`error`). |
| `feature_flag_cache_hits_total` / `feature_flag_cache_misses_total` | — | In-process snapshot cache behavior. |
| `feature_flag_snapshot_stale_total` | — | Evaluations served from a stale snapshot during a DB outage. |
| `feature_flag_snapshot_fallback_total` | — | Evaluations that used env/compiled defaults because no snapshot was usable. |
| `feature_flag_snapshot_age_seconds` | — | Age of the snapshot currently used. |
| `feature_flag_invalidations_total` | — | Local snapshot invalidations after admin writes. |
| `events_published_total` | `event`, `mode` | Events published locally or durably. |
| `events_handled_total` | `event`, `outcome` | Event handler calls (`success`, `error`, `panic`). |
| `events_enqueue_errors_total` | `event` | Durable event jobs that could not be enqueued. |
| `scheduler_runs_total` | `schedule`, `outcome` | Scheduler slots processed (`enqueued`, `skipped`, `duplicate`). |
| `scheduler_leader` | — | `1` on the process holding the scheduler lease. |
| `scheduler_enqueue_errors_total` | `schedule` | Scheduled jobs that could not be enqueued. |
| `scheduler_runs_purged_total` | — | Scheduler run-ledger rows deleted by retention. |

The default Prometheus registry also exposes Go runtime and process metrics
(for example `go_goroutines`, `process_resident_memory_bytes`).

Useful queries:

```promql
# 5xx ratio
sum(rate(http_requests_total{status=~"5.."}[5m]))
  / sum(rate(http_requests_total[5m]))

# p95 latency by route
histogram_quantile(0.95,
  sum by (le, path) (rate(http_request_duration_seconds_bucket[5m])))

# New dead letters
increase(jobs_dead_total[15m]) > 0

# Sustained claim failures
rate(jobs_claim_errors_total[5m]) > 0

# No scheduler leader in the fleet (expected value is 1 overall, not per pod)
sum(scheduler_leader) != 1

# Scheduled slots failing to enqueue
increase(scheduler_enqueue_errors_total[15m]) > 0

# Durable event fan-out failures
increase(events_enqueue_errors_total[15m]) > 0
```

There is no queue-depth gauge. Query PostgreSQL for backlog and age:

```sql
SELECT kind, status, count(*) AS jobs,
       min(run_at) AS oldest_due,
       max(updated_at) AS newest_update
FROM jobs
WHERE status IN ('queued', 'running')
GROUP BY kind, status
ORDER BY jobs DESC;

SELECT count(*) AS dead_last_hour
FROM jobs
WHERE status = 'dead' AND updated_at > now() - interval '1 hour';
```

Alert on symptoms (5xx, latency, dead letters, claim errors, audit failures)
and on queue age, not only on raw queue depth. A large but draining queue is
different from a stuck one.

### Feature flags

Feature-flag evaluation falls back in this order: last good snapshot (up to
`FEATURE_FLAGS_MAX_STALENESS_SECONDS`), then `FEATURE_FLAGS` environment
values, then compiled defaults. `FEATURE_FLAGS_FORCE_OFF` and a flag's kill
switch always win. Check `feature_flag_snapshot_loads_total{result="error"}`,
`feature_flag_snapshot_fallback_total`, and
`feature_flag_snapshot_age_seconds` when behavior diverges between replicas.

Admin writes invalidate only the replica that served the request; other
replicas converge within `FEATURE_FLAGS_CACHE_TTL_SECONDS` (30s by default).
For an immediate global stop, set `FEATURE_FLAGS_FORCE_OFF` or the kill switch
through the admin API. A database outage does not prevent force-off. See
[Feature Flags](feature-flags.md) for the API and precedence details.

## 3. Logs and request IDs

`APP_ENV=production` and `staging` use JSON `slog` output; other environments
use text. `LOG_LEVEL` is `debug`, `info`, `warn`, or `error` (default `info`).
Logs go to stdout; collect them with the platform's normal log pipeline.

The request logger emits one record per request with:

`method`, `path`, `status`, `latency`, `ip`, `request_id`, `user_id`,
`content_length`, `user_agent`, and `query`. When tracing is enabled and the
request is sampled, records also include `trace_id` and `span_id`.

- `status` is the effective status after the Fiber error handler; a handler
  returning an error is not logged as `200`.
- 4xx requests log at `WARN`; 5xx log at `ERROR`; the rest log at `INFO`.
  A request whose client disconnected (`context.Canceled`) is reported as
  `499 client_closed_request` at `INFO` and is not counted as a 5xx.
- Query values for keys named `code`, `state`, `password`, or containing
  `token` or `secret` are redacted. Do not rely on this to protect other
  sensitive parameters; do not put secrets in URLs.
- Request and response bodies are not logged. Keep it that way.

`X-Request-Id` is accepted from a client only if it is 1–128 characters of
`[A-Za-z0-9._:-]`; otherwise the API generates a UUID. The value
is echoed on the response and included in RFC 9457 problem responses as
`requestId`. Use it to correlate logs, audit events, and client reports:

```bash
# JSON logs
... | jq -c 'select(.request_id=="<id>")'

# Plain logs
grep 'request_id=<id>' app.log
```

When OpenTelemetry tracing is enabled, sampled request records also carry
`trace_id` and `span_id`. Search by `request_id` first, then use `trace_id` in
the tracing backend to inspect the HTTP span, its sanitized PostgreSQL spans,
and any job spans linked to the request. Tracing never adds emails, tokens,
payloads, SQL text, headers, or raw error messages to logs or spans; see
[Tracing](tracing.md).

Audit events carry the same request ID and the resolved tenant ID. Mutations
are audited in two tiers:

- **Tier 1** services write a terminal event with `RecordTx` inside the
  mutation transaction. If the insert fails, the transaction rolls back and the
  API returns `503 system.audit_unavailable`. These are the identity
  registration/profile/password/delete operations, admin role/ban/unban, and
  admin file delete/restore. A successful `audit.RecordInTx` marks the
  request, so the generic fallback middleware does not write a second row. A
  service built without a recorder fails closed with the same 503.
- **Tier 2** routes run `audit.Guard` after idempotency. The guard inserts an
  `outcome=started`, `source=http` intent row before the handler and completes
  it as `succeeded`, `failed`, or `denied` with the HTTP status/error code. A
  required guard returns the same 503 when the intent cannot be stored, so the
  handler never runs unaudited. Completion failures are counted and leave a
  stale `started` row for reconciliation; they cannot roll back a committed
  mutation.

Idempotent replays are skipped so a replay does not duplicate the trail, and
audit rows are never cached with the idempotency response. When no tenant
middleware ran (for example an internal call or a pre-tenant test), the event
uses the compatibility tenant id 1. `audit_events` is append-only: only a
started HTTP intent can transition to a terminal outcome, and only completion
fields may change. Alert on `audit_intent_failures_total`,
`audit_completion_failures_total`, `audit_tx_missing_total`, and
`audit_domain_missing_total`; investigate rows where
`outcome = 'started' AND created_at < NOW() - INTERVAL '10 minutes'`. See
[Module Template](module-template.md#5-audit) for explicit domain events; set
`Event.TenantID` there or let the recorder resolve it from the context.

## 4. Background jobs

`internal/platform/jobs` is a PostgreSQL-backed durable queue. The API hosts
the worker in-process; every replica runs a worker with concurrency 4. Claims
use `FOR UPDATE SKIP LOCKED`, so multiple replicas are safe.

Every job is tenant-owned. `Queue.Enqueue` and `Queue.EnqueueTx` resolve
`jobs.tenant_id` from the context with `tenant.EffectiveID` (default tenant 1);
an explicit positive `Job.TenantID` is kept. The claim query returns the column
and the worker installs `tenant.WithID(ctx, job.TenantID)` before invoking the
handler, so email and audit handlers run with the tenant that enqueued the job.
A claimed row with no tenant falls back to tenant 1 for compatibility. The
`(tenant_id, status, run_at)` and `(tenant_id, kind, status)` indexes support
tenant-first inspection. Retention and Prometheus labels are deliberately
global: terminal jobs are purged by age regardless of tenant, and the metric
labels stay `kind` only to avoid cardinality growth.

Enqueue captures the active W3C `traceparent` in `jobs.trace_parent` when the
caller has a recording span. Each attempt runs as a new-root `job.process`
span with a link to the enqueue span, plus `job.id`, `job.kind`, tenant,
attempt, and outcome attributes. The handler context carries the span, so
sanitized database spans nest under the job. `NULL`/malformed traceparents are
ignored. See [Tracing](tracing.md) and the schema note in section 6.

`Queue.EnqueueOnce`/`EnqueueOnceTx` use the nullable
`jobs.idempotency_key` and a partial unique `(tenant_id, idempotency_key)`
index. A duplicate returns the existing id and inserts nothing, so a producer
retry (or an event fan-out retry) cannot create a second row. Plain
`Enqueue`/`EnqueueTx` do not suppress duplicates. Terminal rows keep their key
until retention removes the row.

### Lifecycle and retries

- A job is `queued` -> `running` -> `succeeded` or `dead`. `attempts` increments
  at claim time.
- A handler error requeues the job with exponential backoff (1s doubling, capped
  at 1h) while `attempts < max_attempts` (default 10). When the budget is
  exhausted, the job becomes `dead` and its payload is cleared.
- An unregistered job kind follows the same path. A rollout can register the
  handler before pending work dies, but a permanently missing handler eventually
  dead-letters.
- Handlers get a 10m timeout by default. Cancellation is cooperative; a handler
  that ignores its context keeps running and keeps heartbeating.
- A running job renews a 5m lease every 30s. If a process crashes, the next
  claim by any worker requeues the expired job. A reclaimed job consumes an
  attempt. The previous owner may still be executing the old handler, so
  duplicate concurrent execution is possible; handlers must be idempotent.
- On SIGINT/SIGTERM the worker stops claiming and cancels running handlers
  while HTTP drains concurrently, all inside the shared 20s shutdown budget
  (see [Graceful shutdown](#graceful-shutdown)). A non-cooperative handler can
  exceed that window. A handler that fails while the worker is
  stopping is requeued immediately **without consuming its attempt**
  (`last_error` starts with `jobs: interrupted by worker shutdown`, span
  outcome `released`), so a drain during the final attempt never
  dead-letters the job. A handler that still succeeds is recorded normally.
- The worker and scheduler identity (`locked_by`, lease `holder`) is
  `<hostname>-<pid>-<random>`, so replicas with the same hostname never share
  a lease identity.

Delivery is **at-least-once**, never exactly-once. Duplicate emails and
duplicate side effects must be expected.

### Dedicated workers for slow kinds

One worker processes every registered kind by default, so a burst of slow jobs
(for example large exports) can occupy every slot and delay fast ones such as
`email.send` (head-of-line blocking; watch `jobs_queue_lag_seconds{kind}`).
`jobs.WithKinds(kinds...)` restricts a worker's claim query to the listed
kinds and `jobs.WithExcludeKinds(kinds...)` skips them, so a deployment can run

```go
slow := jobs.NewWorker(pool, instanceID+"-slow", jobs.WithKinds("reports.render"), jobs.WithConcurrency(2))
general := jobs.NewWorker(pool, instanceID, jobs.WithExcludeKinds("reports.render"))
```

in one process or in separate deployments. Both filters only narrow which due
jobs are claimed; expired-lease recovery stays global, so any worker requeues
a crashed job of any kind (it never executes it). Register a handler for every
kind a worker claims, and make sure every kind is claimed by some worker:
an excluded kind with no dedicated worker simply waits in the queue.

### Notification email

Notifications use the same queue and `email.send`/`email.send.encrypted` kinds
as identity email. The notifications service resolves the recipient through
identity's `contracts.NotificationRecipientLookup`; it never reads `users`
directly. When the `email` channel is enabled, it inserts the in-app row and
enqueues the email job in one transaction, so a queue outage returns `500` and
rolls the in-app row back. Channel preferences are applied before any write: an
explicit `enabled=false` row skips that channel, while a missing row means the
channel is enabled.

Inspect the feed and preferences:

```sql
-- Unread backlog per tenant/user
SELECT tenant_id, user_id, count(*) AS unread,
       max(created_at) AS newest
FROM notifications
WHERE read_at IS NULL
GROUP BY tenant_id, user_id
ORDER BY unread DESC;

-- Explicit channel opt-outs
SELECT tenant_id, user_id, type, channel, enabled, updated_at
FROM notification_preferences
WHERE enabled = false
ORDER BY updated_at DESC;
```

Notification emails have no links or secrets, so dead-lettered rows only need a
re-send through the admin endpoint or the producing module. The generic audit
trail records successful `POST /notifications/:id/read`,
`POST /notifications/read-all`, and `PUT /notification-preferences` requests;
`POST /notifications/send` is guarded by a required `notification.sent`
intent row (503 when the audit store is down). Replays are skipped.

### Retention and payload clearing

Workers purge `succeeded` and `dead` rows older than 30 days, hourly by
default; the first pass runs when a worker starts. Queued and running rows are
never purged. Each pass is bounded to 5s and deletes at most 1,000 rows per
batch, so a large backlog cannot hold one transaction open. The partial index
`idx_jobs_terminal_updated_at ON jobs (updated_at) WHERE status IN
('succeeded','dead')` serves the purge's predicate and ordering.

Successful and dead jobs have `payload = '{}'`. This prevents reset and
verification links from lingering in a dump, but it also means **a dead job
cannot be replayed from its row**. Re-trigger the business flow (for example ask
the user to request another reset email) instead of blanket-updating dead rows
back to `queued`.

### Draining and JWT rotation

`JWT_SECRET` also derives the AES-256-GCM key that seals queued email payloads.
Rotating it has two effects:

1. Existing access tokens fail signature validation and must be reissued.
2. Encrypted jobs already in `jobs` can no longer be decrypted.

Refresh tokens are opaque database rows, not JWTs, so rotation does **not**
revoke them; a client holding a valid refresh token can mint a new access token
signed with the new secret. If the rotation must force a full re-login, revoke
refresh tokens (or bump the user's token version) separately. The queue drain
below is still required because of the encrypted job payloads.

A rolling update that briefly mixes old and new secrets can let an old worker
claim a job it cannot decrypt, or a new worker claim one encrypted with the old
key. Drain first:

1. Stop new email-producing traffic (or pause the producer) and let the queue
   settle. Watch `queued` and `running` counts until both are zero.
2. Suspend/delete the HPA and scale the Deployment to 0 so no worker is running
   during the secret swap. Do not rely on a rolling update for this rotation.
3. Update the Secret and restart/scale the Deployment back up.
4. Verify `/readyz`, then send a test registration/reset flow and confirm the
   job reaches `succeeded`.

If draining is impossible, expect failures and inspect `dead` rows after the
rotation. Because terminal payloads are cleared, recovery means re-triggering
the user flow, not requeueing the old row. See
[deploy/k8s/README.md](../deploy/k8s/README.md#jwt_secret-rotation-and-the-job-queue).

### Inspecting jobs

```sql
-- Backlog by tenant, kind, and state
SELECT tenant_id, kind, status, count(*), min(run_at), max(updated_at)
FROM jobs
WHERE status IN ('queued', 'running')
GROUP BY tenant_id, kind, status
ORDER BY count(*) DESC;

-- Recent failures and dead letters
SELECT id, tenant_id, kind, status, attempts, max_attempts,
       idempotency_key, last_error, updated_at
FROM jobs
WHERE status IN ('running', 'dead')
ORDER BY updated_at DESC
LIMIT 50;

-- Lease age for running jobs
SELECT id, kind, locked_by, locked_at,
       now() - locked_at AS lease_age, attempts, max_attempts
FROM jobs
WHERE status = 'running'
ORDER BY locked_at;
```

A stuck `running` job usually recovers when its lease expires and another
worker polls. Before any manual intervention, confirm no live process owns it.
As a last resort, and accepting duplicate execution, requeue one job only after
the lease is far past expiry:

```sql
UPDATE jobs
SET status = 'queued',
    locked_at = NULL,
    locked_by = NULL,
    run_at = now(),
    updated_at = now()
WHERE id = <id>
  AND status = 'running'
  AND locked_at < now() - interval '10 minutes';
```

Never bulk-clear locks, and never requeue `dead` jobs (their payload is gone).

### Durable events

`internal/platform/events` publishes metadata-rich events locally and/or as
jobs. A durable subscription adds one job per event and handler, with
`jobs.idempotency_key = event:<eventId>:<handler>:<tenantId>`. The job payload
is the event JSON, so `jobs.payload` contains the event data. Delivery is
at-least-once; handlers must be idempotent. Local handler panics are recovered
and combined with `errors.Join`; check `events_handled_total{outcome="panic"}`
and `events_enqueue_errors_total` during an incident. Durable event handlers
must be registered before `worker.Run`; the app does this after all
`EventProvider` modules register.

### Scheduler

`internal/platform/scheduler` is enabled with `SCHEDULER_ENABLED=true` and runs
in-process in every API replica. Only the holder of the single
`scheduler_leases` row evaluates schedules. `scheduler_runs` is the unique
`(schedule, scheduled_for)` ledger: the ledger row and the job enqueue share a
transaction, so a leader restart or overlapping election cannot double-enqueue
a slot. The lease is renewed every `SCHEDULER_HEARTBEAT_SECS`; a failed renewal
cancels the current pass and steps down.

- `SCHEDULER_CATCHUP`: `none` skips slots missed while no leader ran, `latest`
  runs only the newest, `all` replays in order up to
  `SCHEDULER_CATCHUP_LIMIT` per poll. A slow poll during continuous leadership
  still runs the latest slot under `none`, so short schedules are not starved.
- `SCHEDULER_RETENTION_DAYS` bounds `scheduler_runs`, but the newest row of
  every schedule is always kept: it anchors catch-up, and deleting it would
  make a sparse schedule (for example `@yearly`) look never-run and re-enqueue
  its already executed slot.
- A schedule's job payload is `{"schedule","scheduledFor","data"}` and its key
  is `scheduler:<name>:<slot>`. The job kind must have a worker handler; a
  missing handler follows the normal retry/dead-letter path.
- Scheduler logs include `holder`, `schedule`, `slot`, and the underlying
  error. `scheduler_leader` should sum to 1 across replicas; if it is 0, check
  database reachability and lease expiry. If it is greater than 1, check clock
  skew and whether a process is running with an extremely short lease TTL.

```sql
-- Current leader and lease age
SELECT name, holder, expires_at, now() - heartbeat_at AS since_heartbeat
FROM scheduler_leases;

-- Recent slots and whether they enqueued or were skipped
SELECT schedule, scheduled_for, status, job_id, error, created_at
FROM scheduler_runs
ORDER BY scheduled_for DESC
LIMIT 50;

-- Per-schedule backlog of due but unledgered work (manual check)
SELECT kind, count(*), min(run_at)
FROM jobs
WHERE status = 'queued'
GROUP BY kind
ORDER BY count(*) DESC;
```

## 5. Webhooks

Outbound deliveries reuse the durable jobs queue under the
`webhook.delivery` kind. Producers with a transaction (registration, file
upload/delete) enqueue a `webhook.dispatch` fan-out job in that transaction;
the fan-out commits a `webhook_deliveries` row and its first job atomically per
endpoint; the worker sends one signed attempt and schedules the next attempt as
a new job. Delivery is at-least-once, so receivers must deduplicate on
`X-Webhook-Delivery`. The signature covers
`<X-Webhook-Id>.<timestamp>.<X-Webhook-Event>.<body>` (a breaking change from
the earlier `<timestamp>.<body>`; see [Webhooks](webhooks.md)).

### Delivery state and retries

- `pending` is queued or awaiting the next attempt; `next_attempt_at` doubles as
  the claim lease while an attempt is in flight.
- `succeeded` means a 2xx response. `dead` is terminal after a permanent
  failure or an exhausted attempt budget.
- `2xx` succeeds; `3xx` is permanent (redirects are not followed); `4xx` is
  permanent except `408`, `425`, `429`; `5xx` and transport errors retry.
- Backoff is exponential from `WEBHOOK_RETRY_BASE_SECS` to
  `WEBHOOK_RETRY_MAX_SECS`. A valid `Retry-After` (seconds or HTTP date) can
  only lengthen the backoff (`0`/past dates do not retry immediately) and is
  capped at the same maximum.
- Disabled or deleted endpoints are never claimed; their pending deliveries,
  and pending deliveries whose attempts are exhausted, become `dead` on the
  next job instead of staying `pending`.
- Each attempt uses the endpoint's `timeoutMs` (snapshotted on the delivery),
  bounded by `WEBHOOK_REQUEST_TIMEOUT_SECS`. The attempt and its outcome are
  recorded with a detached 5s context even during shutdown.
- Every attempt is recorded with status code, duration, and a bounded error.
  Response bodies are not stored.
- Endpoint edits/rotation do not change queued deliveries: URL, payload, event
  timestamp, attempt budget, and the sealed secret are snapshotted at dispatch.
  Replay creates a new delivery with the endpoint's current URL and active
  secret.

### Inspecting deliveries

```sql
-- Backlog by tenant/status
SELECT tenant_id, status, count(*), min(next_attempt_at), max(updated_at)
FROM webhook_deliveries
GROUP BY tenant_id, status
ORDER BY count(*) DESC;

-- Dead deliveries
SELECT id, tenant_id, endpoint_id, event_type, attempts, max_attempts,
       last_status_code, last_error, updated_at
FROM webhook_deliveries
WHERE status = 'dead'
ORDER BY updated_at DESC
LIMIT 50;

-- Attempt timeline
SELECT attempt, status_code, duration_ms, error, created_at
FROM webhook_delivery_attempts
WHERE delivery_id = $1
ORDER BY attempt;

-- Pending work and its jobs
SELECT d.id, d.tenant_id, d.next_attempt_at, j.id AS job_id, j.status, j.run_at
FROM webhook_deliveries d
LEFT JOIN jobs j
  ON j.kind = 'webhook.delivery'
 AND j.payload->>'deliveryId' = d.id::text
WHERE d.status = 'pending'
ORDER BY d.next_attempt_at
LIMIT 50;
```

A pending delivery with a future `next_attempt_at` and no live job can happen
after a crash in the narrow window between the HTTP attempt and bookkeeping.
The original job lease normally retries it. Otherwise replay the delivery
instead of bulk-editing state.

### Secrets and key rotation

Outbound endpoint secrets (`whsec_...`) and inbound source secrets
(`whin_...`) are sealed with AES-256-GCM. The key comes from
`WEBHOOK_SECRET_ENCRYPTION_KEY`; local/test fall back to `JWT_SECRET`, while
production **requires** a dedicated key of at least 32 characters.

Rotating `WEBHOOK_SECRET_ENCRYPTION_KEY` makes every stored webhook secret
unreadable. Plan it as a secret reset, not a transparent key roll:

1. Announce a receiver maintenance window.
2. Deploy the new encryption key.
3. Rotate every endpoint secret and inbound source secret; deliver the new
   plaintext values to the owners.
4. Expect in-flight outbound deliveries encrypted under the old key to become
   `dead` with a decryption error; replay them after rotation if needed.
5. Inbound requests fail `401` until their senders use the new secrets.

Endpoint secret rotation itself is safe for in-flight work because queued
deliveries keep their snapshot. Inbound rotation is immediate: the old secret
stops verifying on the next request.

### SSRF allowlist

`WEBHOOK_SSRF_ALLOWLIST` lets trusted private receivers through. Entries are
comma-separated IPs, CIDRs, or exact hostnames. The default (empty) blocks
private, loopback, link-local, multicast, CGNAT, and reserved targets, pins DNS
results to vetted IPs, disables environment proxies, and never follows
redirects. Do not add broad ranges to make one receiver work; add the exact
host/IP/CIDR and keep the list audited.

### Retention

An hourly loop (first pass at startup) deletes only terminal deliveries
(`succeeded`/`dead`) and inbound events older than
`WEBHOOK_RETENTION_DAYS`. Pending deliveries and endpoint/source
configuration are never purged. Delivery attempts cascade with their delivery;
endpoints and inbound sources are soft-deleted and their deliveries/events use
`ON DELETE RESTRICT`, so history only leaves through this purge. Purges delete in batches of at most 1,000 rows until drained (30s
per pass) using the non-tenant-leading retention indexes described in
[Webhooks](webhooks.md).

### Receiver troubleshooting

- `404`: source ID unknown, disabled, or soft-deleted, or its tenant is
  suspended; verify the configured `receiverUrl`.
- `401` with `webhook signature is required`: missing timestamp/signature.
- `401` with stale/invalid timestamp: check clock skew against
  `WEBHOOK_TIMESTAMP_TOLERANCE_SECS`.
- `401` with invalid signature: confirm the sender signs
  `<X-Webhook-Id>.<timestamp>.<X-Webhook-Event>.<exact raw body>` (raw header
  values; empty event if the header is absent) with the active source secret,
  not a rotated one. Senders still signing the old `<timestamp>.<body>` input
  fail here.
- `200` with `duplicate: true`: the event was already stored for
  `(source, X-Webhook-Id)`; this is success.
- Inbound events are stored under the source tenant and audited as
  `webhook.inbound.received`; `X-Tenant-Slug` is ignored on the public route.

## 6. Idempotency and Redis

Idempotent mutations are opt-in with `Idempotency-Key` (1–128 chars of
`[A-Za-z0-9._:-]`). The cache stores:

| Key | TTL | Purpose |
| --- | --- | --- |
| `idempotency:<method>:<path>:tenant=<id>:<slug>:<identity>:<key>:lock` | 2 minutes | Owner-token lock for the in-flight request. |
| `idempotency:<method>:<path>:tenant=<id>:<slug>:<identity>:<key>:response` | 24 hours | Cached 2xx status, body, and selected headers for replay. |

`identity` is the authenticated user ID, or the client IP for public routes.
Keys are scoped by method, path, tenant ID/slug, and identity, so the same
client key/body/IP in another tenant cannot replay a cross-tenant `201`.
Existing keys from before tenant scoping stop matching and expire within 24h.

Behavior to remember:

- Only 2xx responses are cached. Non-2xx responses are not replayed.
- Non-multipart bodies up to 1 MiB are body-bound with SHA-256. Reusing a key
  with a different body returns `409`; multipart and larger bodies are not
  bound.
- Concurrent duplicates while the lock is held return `409`. After the owner
  completes, a retry replays the cached response with
  `Idempotent-Replayed: true`.
- Cache read/write/lock failures **fail open**: the handler still runs and the
  client gets the normal response. A cache outage can therefore cause duplicate
  mutations. That is the deliberate availability trade-off; clients should
  still send stable keys.
- Audit skips replayed mutations.
- Cached bodies may contain personal data and live for 24 hours. Size Redis
  accordingly, restrict access, and account for eviction. If an entry is
  evicted, a later retry executes again.
- Scope `<path>` is the matched route template plus its parameter values, so
  `/Users/7/ban/` and `/users/7/ban` (same route under case-insensitive,
  non-strict routing) share one scope. Keys written before this change stop
  matching and expire within 24h.
- Memory cache is per process. It is not suitable for multiple replicas or for
  replay/lock guarantees across restarts. Production config rejects
  `CACHE_DRIVER=memory` unless `CACHE_ALLOW_MEMORY_IN_PRODUCTION=true`, which
  is only for a deliberate single-replica deployment.
- Rate limits are fixed windows aligned to epoch multiples of the tier window.
  Each request runs one atomic `INCR` (+`PEXPIRE` on first hit) on
  `ratelimit:<tier>:<apikey|user|ip>:<id>:<windowStart>`, so replicas sharing
  Redis cannot race. A client can send up to twice the limit across a window
  boundary. Responses carry `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
  `X-RateLimit-Reset`, and `Retry-After` on `429`.
- Rate-limit cache errors fail open: the request is allowed, a warning is
  logged, and `rate_limit_errors_total{tier}` increments. Login lockouts are a
  separate, fail-closed mechanism and are not relaxed by a cache error.

Operational checks:

```bash
redis-cli -u "$REDIS_URL" PING
redis-cli -u "$REDIS_URL" INFO keyspace
redis-cli -u "$REDIS_URL" INFO stats | grep -E 'evicted_keys|expired_keys'
redis-cli -u "$REDIS_URL" --scan --pattern 'idempotency:*:response' | head
redis-cli -u "$REDIS_URL" TTL '<one-key-from-the-scan>'
```

Avoid `KEYS *` on a production Redis and **do not use `FLUSHALL`/`FLUSHDB`** as
a routine fix. Flushing discards idempotency replays, rate-limit counters, and
session cache state; it does not reset JWT validity, but it removes operational
state and can cause duplicate mutations or a burst of database-backed session
validation. Eviction is also destructive to idempotency: configure
`maxmemory-policy noeviction` (or a policy with enough headroom) and alert on
`evicted_keys` if the provider exposes it.

During a Redis outage:

- `/readyz` reports `cache: down` with `status:"degraded"` and still returns
  `200`, so pods stay in the Service while PostgreSQL is healthy.
- Session validation falls back to PostgreSQL, but cache invalidation writes
  can fail. Bans, password changes, and role changes made during the outage may
  not propagate to every replica immediately. Re-check security-sensitive
  accounts after recovery.
- Idempotency and rate limits fail open. Expect duplicate mutations and weaker
  abuse protection until the cache returns.
- Do not switch `CACHE_DRIVER` in place; environment changes require a restart.
  Moving to memory is an emergency, single-replica-only degradation and loses
  shared state. Revert as soon as Redis is healthy.

## 7. Migrations and legacy upgrade

Only `cmd/migrate` applies DDL. `cmd/api` and `cmd/seed` assume the schema
exists, so replicas do not race. `cmd/migrate` loads only `DB_*` plus
`APP_ENV`/`LOG_LEVEL` for validation and logging; it needs no JWT, admin, SMTP,
storage, or other application secrets, and it rejects plaintext `DB_SSLMODE`
values in production.

### Normal flow

```bash
# Local: apply pending migrations
go run ./cmd/migrate

# Or through the Makefile
make migrate-run
```

Docker Compose runs the one-shot `migrate` service before the app. Kubernetes
uses a separate Job and ordered flow (see below).

### Fresh installs vs deployed databases

- A fresh install uses `migrations/000001_init_schema.up.sql`.
- Once a database is deployed, never edit `000001`. Add a migration with only
  the new objects: `make migrate-create module=<owner> name=<purpose>` writes
  `<UTC YYYYMMDDHHMMSS>_<owner>_<purpose>.{up,down}.sql` (golang-migrate
  `-format 20060102150405 -tz UTC`). Timestamp versions do not collide when
  branches each add a migration and are always above legacy versions 1..3.
  golang-migrate only applies versions above the recorded one, so a branch
  migration older than main's newest must be regenerated before merge.
  `test/arch/migrations_test.go` (part of `make test`/CI) rejects duplicate
  versions, missing up/down pairs, non-timestamp versions, and unknown
  owners.
- A database that applied the old `000001`–`000003` history must keep that
  history. Restore the old source files from git history (golang-migrate needs
  the recorded version to exist), add a timestamped migration, and run
  `cmd/migrate`. Never point an old database at the consolidated `000001` and
  never reset `schema_migrations` to force it through.
- If a fresh schema was created out-of-band and already matches the baseline,
  verify the objects and baseline the version (for example `force 1`) instead
  of applying the file over populated tables.

### Multi-tenant foundation upgrade

The baseline now creates `tenants`, seeds tenant id 1 (`default`), adds
`tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)` to users, files,
notifications, notification preferences, refresh/reset/verification tokens,
jobs, and audit events, adds tenant-leading indexes, and scopes the last-admin
trigger per tenant. A database deployed before this change does **not** receive
those objects from the edited baseline. Create a timestamped migration
above its recorded version (or apply reviewed DDL manually) that:

1. creates `tenants` and inserts `('default','Default')` first so it gets id 1;
2. adds/backfills the `tenant_id` columns (default 1) and foreign keys;
3. rebuilds user uniqueness as `(tenant_id, lower(email))` and
   `(tenant_id, google_id)` for active rows, and adds the tenant-leading
   indexes;
4. replaces `protect_last_admin()` with the per-tenant body;
5. adds `feature_flags`, `feature_flag_targets`, their partial unique indexes,
   and a unique index on `users(tenant_id, id)` for tenant-safe user targets.

Keep tenant id 1 as the compatibility anchor unless you also change the column
defaults and backfill every row. The notifications schema is additive:
create `notifications` and `notification_preferences` with tenant/user
columns, the feed/unread indexes, and the unique preference index. After the
upgrade, run `cmd/migrate`; it verifies the tenant table/columns/indexes,
`tenant_id` on `jobs`, `audit_events`, and `feature_flag_targets`, the
feature-flag tables/indexes, the notification tables and unique preference
index, and the default tenant row.

`TENANT_DEFAULT_SLUG` (default `default`) controls which tenant header-less
requests resolve to in local/test compatibility mode; `TENANT_REQUIRE_HEADER=true`
rejects requests that omit `X-Tenant-Slug`. **Production requires it**, and the
Kubernetes ConfigMap sets it, so no HTTP write can silently land in tenant 1.
Roll it out in lower environments only after all clients send the header.
Startup checks existence separately from request resolution: a suspended
configured/default tenant does not fail `cmd/api` or `cmd/seed`, but its
requests return 404 until an operator reactivates it. Browser OAuth start and
reset/verify links may select the tenant with `?tenant=<slug>` because a header
cannot be sent on navigation; the header still wins when both are present.
`cmd/migrate` verifies `tenants.slug/status`, the tenant slug uniqueness, and
that every tenant-owned query index leads with `tenant_id`.
Identity, files, jobs, and audit now carry/propagate `tenant_id`; identity and
files queries are tenant-scoped. Existing rows default to tenant 1 and their old
`<userID>/<uuid>.<ext>` storage keys remain valid because downloads read
`files.storage_path`. New uploads use
`<tenantID>/<userID>/<uuid>.<ext>`; if you migrate existing blobs, copy first and
update `files.storage_path` in the same operation. RLS and per-tenant limits are
future work; see the README scope note.

`cmd/migrate` checks for a dirty migration state before running, and after
migrating verifies required tables, columns, unique indexes, `tenants.slug`/
`status`, and that each tenant-owned query index starts with `tenant_id`. A
legacy partial schema or a same-named global index fails with an actionable
error instead of silently serving a broken database.

### Jobs `trace_parent`

The consolidated baseline includes `jobs.trace_parent TEXT`. This boilerplate
has no deployed database, so no numbered migration was added. If a database
already applied version 1, run the additive statement once before deploying
this build:

```sql
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS trace_parent TEXT;
```

`cmd/migrate` verifies the column and fails with an actionable error if it is
missing. Existing rows keep `NULL`, which produces a root `job.process` span
without an enqueue link. The column stores only a W3C `traceparent` header; it
does not contain the job payload or tenant data.

### Failure and backup/restore

```bash
# What version is recorded?
psql "$DSN" -c 'TABLE schema_migrations;'

# Inspect a Kubernetes migration Job
kubectl -n <namespace> describe job/<migration-job>
kubectl -n <namespace> logs job/<migration-job>
```

- A dirty migration means the previous attempt failed mid-transaction. Stop the
  rollout, read the database logs, and repair forward. Do not blindly `force`
  a version or rerun a partially applied DDL script.
- Jobs are immutable. Delete a failed/completed Job before applying a changed
  template, but only after confirming no migration is running.
- Production rollback is forward-only in this setup. A previous image may not
  understand a newer schema. Prefer backward-compatible migrations and test
  the rollback before deployment.
- A migration is not a backup. Take a verified snapshot or PITR restore point
  before destructive or long-running changes, and test the restore path.
  For a restore, stop writes, restore the database, verify
  `schema_migrations`, then start the API and let jobs catch up.
- Keep migrations short. Backfills and large rewrites belong in jobs; use
  expand/contract for incompatible columns so old and new pods can coexist
  during a rolling update.

## 8. Kubernetes deployment notes

Follow [deploy/k8s/README.md](../deploy/k8s/README.md) for the full commands.
The operational invariants:

1. Apply the environment Namespace and ConfigMap first:
   `kubectl apply -k deploy/k8s/overlays/<env>/prereqs`. This applies the
   environment ConfigMap values before the migration Job.
2. Create `fiber-boilerplate-secrets` in that namespace out of band (or through
   External Secrets / Sealed Secrets / SOPS). It is never part of a
   kustomization.
3. If a migration Job already exists, confirm it is `Complete` or `Failed`
   before deleting it; never interrupt a running migration. Apply
   `kubectl apply -k deploy/k8s/overlays/<env>/migration`, wait for
   `condition=complete`, and read its logs. The deploy workflow refuses to
   delete an unfinished Job and fails immediately on `Failed=True` rather than
   waiting for its 15-minute deadline.
4. Only then apply the app overlay: `kubectl apply -k deploy/k8s/overlays/<env>`
   and `kubectl -n <namespace> rollout status deployment/fiber-boilerplate`.
   `kubectl apply -k` has no cross-resource ordering guarantee, and the
   migration Job is deliberately excluded from every app overlay.
5. The app overlay and `prereqs` overlay both use the same environment
   ConfigMap component; keep `overlays/<env>` and
   `overlays/<env>/migration` on the same image tag.

Other caveats:

- **HPA owns replicas.** The Deployment intentionally has no `spec.replicas`.
  Do not add it back or the HPA and Deployment controller will fight. The HPA
  needs metrics-server; `<unknown>` targets mean metrics are unavailable.
  Staging scales 2–4 replicas; production scales 3–10.
- **PDB:** `minAvailable: 1`. Every environment runs at least two replicas, so a
  node drain can proceed one pod at a time. Do not lower an HPA minimum below 2
  without also changing the PDB.
- **NetworkPolicy probes are CNI-dependent.** Some CNIs allow kubelet probes
  regardless of policy; others drop them. If probes fail, use the commented
  node/node-pod CIDR rule, never `0.0.0.0/0`. A Prometheus scrape also needs an
  ingress rule for the monitoring namespace; it is commented in the manifest.
- **Config changes are not hot-reloaded.** `envFrom` is read at container
  start. After editing the environment ConfigMap component or Secret, run
  `kubectl rollout restart deployment/<name>`.
- **SMTP:** the app uses STARTTLS, normally port 587. Implicit TLS on port 465
  is not supported. Set `EMAIL_DRIVER=smtp`, `SMTP_HOST`, `SMTP_PORT`,
  credentials, and `EMAIL_FROM_ADDRESS`; production requires
  `SMTP_REQUIRE_TLS=true`, and `EMAIL_DRIVER=console` is rejected. A relay that
  does not advertise STARTTLS will fail sends.
- **S3/MinIO:** set `STORAGE_DRIVER=s3` (or `minio`), endpoint, region, bucket,
  access key, and secret key; enable `STORAGE_S3_USE_SSL` in production. Every
  replica checks bucket existence at startup and may try to create a missing
  bucket, so pre-create it to avoid races; an unreachable endpoint or bad
  credentials prevents startup. After startup, object reads/writes/deletes fail
  on use and are not covered by `/readyz`. Local storage needs a shared
  ReadWriteMany volume; `emptyDir` is node-local and unsuitable for multiple
  replicas. Blob keys start with `<tenantID>/<userID>/`; preserve that prefix in
  backup/restore tooling and never serve the backing directory or bucket
  publicly.
- **Redis:** `CACHE_DRIVER=redis` and `REDIS_URL` (use `rediss://` when
  available) are required for shared rate limits, session invalidation, and
  idempotency across replicas. Memory cache is single-replica only and is
  rejected in production unless `CACHE_ALLOW_MEMORY_IN_PRODUCTION=true`.
- **Request bodies:** Fiber's `APP_BODY_LIMIT` (12 MiB) is the multipart upload
  ceiling. Every non-multipart body is capped by `APP_JSON_BODY_LIMIT` (default
  1 MiB, must not exceed `APP_BODY_LIMIT`) and rejected with `413
  payload_too_large` before auth, rate limiting, or decoding. Residual risk:
  Fiber buffers the whole body before middleware runs, so a non-multipart
  request (including a chunked body without `Content-Length`) can still make a
  pod read up to `APP_BODY_LIMIT` before rejection, and a client can label a
  non-upload body `multipart/form-data` to reach the larger limit. Keep an
  ingress body limit in front of the API.

### Graceful shutdown

`terminationGracePeriodSeconds: 30` covers the 5s `preStop` sleep plus one
shared 20s shutdown budget (`shutdownBudget` in `cmd/api/main.go`), leaving a
5s margin before SIGKILL. After SIGTERM the process:

1. stops accepting HTTP and drains in-flight requests, concurrently with
   cancelling the scheduler and then the job worker (they only need the pool
   and cache, which stay open);
2. waits for `pkg/async` goroutines (post-commit emails started by handlers)
   once HTTP has stopped, so no new ones appear;
3. flushes spans, bounded by `OTEL_SHUTDOWN_TIMEOUT_SECS` and the remaining
   budget;
4. closes the cache and the database pool.

Every phase uses the same deadline, so a slow phase shortens the later ones
instead of extending the total. A phase that misses the deadline logs a
warning; HTTP forced shutdown sets exit code 1. Raise
`terminationGracePeriodSeconds` and `shutdownBudget` together.
- **Database connections:** `DB_MAX_CONNS` is per pod. Keep
  `DB_MAX_CONNS * maxReplicas` below the database limit; production uses 10
  with HPA max 10, for at most 100 connections. Staging uses 5 with max 4.
- **Migration Job:** its image is set by
  `overlays/<env>/migration/kustomization.yaml`, separate from the app overlay.
  Keep both on the same image tag.
- **Secrets:** never commit a rendered Secret. Use External Secrets, Sealed
  Secrets, SOPS, or the platform secret manager.

### Continuous deployment (GitHub Actions)

`.github/workflows/deploy.yml` is optional and does not deploy anywhere until
you configure it. It supports manual `workflow_dispatch` for staging or
production, automatic production deploys after a **successful** Release
workflow run triggered by a `v*` tag, and a `dry_run` mode that only
renders/uploads manifests. Failed releases never deploy. Deploys are serialized
per environment with `cancel-in-progress: false`.

Required GitHub configuration:

- Environments `staging` and `production` with protection rules (required
  reviewers, wait timers, deployment branches/tags) as appropriate.
- Environment secret `KUBE_CONFIG`: raw kubeconfig YAML or base64-encoded
  kubeconfig for that cluster. Base64 is recommended.
- Optional environment/repository variable `GHCR_IMAGE` (a secret with the same
  name also works): a bare lowercase GHCR repository that must equal the Release
  workflow image `ghcr.io/<owner>/<repo>`. Schemes, tags, digests, ports, and
  colons are rejected. Defaults to `ghcr.io/${{ github.repository }}`.

The workflow applies the prereqs, verifies the pre-created Secret exists,
refuses to delete an unfinished migration Job, runs and awaits the migration Job
(failing immediately on `Failed=True`), applies the app overlay, waits for the
rollout, and smoke-checks `/healthz` and `/readyz` through a port-forward. The
optional `image_preflight` dispatch input checks GHCR for the exact image first;
it uses `packages: read`. Production requires a full semver tag or `sha-<40
hex>`; `latest`, branch names, and placeholders are rejected. Staging accepts
branch tags and always pulls. Promotion means deploying the same image tag to
staging and then production; the workflow does not rebuild the image.

Rollback:

```bash
kubectl -n <namespace> rollout history deployment/fiber-boilerplate
kubectl -n <namespace> rollout undo deployment/fiber-boilerplate
kubectl -n <namespace> rollout status deployment/fiber-boilerplate --timeout=10m
```

The migration Job is not rolled back. If a release applied an incompatible
migration, restore from a verified backup/PITR snapshot or ship a forward fix;
do not blindly roll the image back onto a newer schema.

## 9. Distributed tracing

Tracing is off unless `OTEL_ENABLED=true`. When disabled, `app.Deps.Tracing`
returns a no-op provider and no collector connection is opened. Configuration,
sampling details, and privacy rules are in [Tracing](tracing.md).

What exists when enabled:

- an HTTP server span per request, excluding `/healthz`, `/readyz`, `/health`,
  and `/metrics`; the span name/route uses the Fiber route template, not the
  raw path;
- sanitized pgx client spans for queries that run under a recording parent,
  with SQLSTATE on failure and no SQL text, arguments, or error messages;
- a `job.process` consumer span per attempt, as a new root with a link to the
  enqueue trace and `job.outcome` (`succeeded`/`retried`/`dead`);
- `trace_id`/`span_id` on request logs.

Operational rules:

- The collector is never part of readiness. If it is down, batches fill the
  bounded SDK queue and are then dropped. The API stays ready. Check the
  endpoint, DNS, NetworkPolicy egress 4317, and collector health; do not
  restart the app to "recover" spans.
- Shutdown flushes spans after HTTP, workers, and async goroutines stop,
  bounded by `OTEL_SHUTDOWN_TIMEOUT_SECS` and the remaining shutdown budget. A
  flush failure is logged as a warning and never changes the exit code.
- Incoming `traceparent` is ignored unless `OTEL_TRUST_INCOMING_TRACE=true`
  (and, with `APP_TRUST_PROXY=true`, only from `APP_TRUSTED_PROXIES`), so a
  client cannot force 100% sampling. When trusted, sampling is parent-based:
  an upstream `traceparent` decides. Use `OTEL_TRACES_SAMPLER_ARG` to control
  root sampling.
- Never add attributes containing emails, tokens, headers, query strings,
  payloads, SQL, file names, or raw error text. Use `tracing.MarkError` for
  failures; it records only a bounded `error.type`.
- An unreachable collector at startup is normal and non-fatal. The OTLP/gRPC
  client connects lazily.

## 10. Incident checklist

### First 5 minutes

1. Confirm scope: `/readyz`, pod status, recent rollout/migration, and the
   affected route or job kind.
2. Correlate with `X-Request-Id` (and `trace_id` when tracing is enabled) in
   logs, problem responses, audit rows, and traces.
3. Check the four golden signals plus `jobs_dead_total`,
   `jobs_claim_errors_total`, `scheduler_leader`,
   `scheduler_enqueue_errors_total`, `events_enqueue_errors_total`, and
   `audit_write_failures_total`.
4. Prefer reversible mitigations: roll back the Deployment, restore a
   dependency, or scale capacity. Do not flush Redis, rotate `JWT_SECRET`,
   delete jobs, or force a migration version as a first response.
5. Record the timeline and evidence while it is fresh.

### PostgreSQL is down or saturated

Symptoms: `/readyz` shows `database: down` (503) or `database: busy`
(`degraded`, 200, pool saturated), 5xx responses, session validation
failures, `jobs: claim failed` warnings, rising `jobs_claim_errors_total`.

Actions:

1. Check DNS, network policy, TLS mode, credentials, and provider status.
2. Check connection saturation (`database: busy`) against `DB_MAX_CONNS` and
   the database's own limit (`pg_stat_activity`). Do not raise the pool beyond
   capacity.
3. If failover is in progress, keep pods alive; liveness does not depend on the
   database. Readiness will recover automatically.
4. After recovery, confirm `/readyz`, watch job backlog drain, and check for
   duplicate side effects from retried requests.

### Redis is down or degraded

Symptoms: `/readyz` shows `cache: down` (`degraded`, 200), rising
`rate_limit_errors_total`, rate limits stop rejecting,
idempotency replay/locks fail open, session invalidation warnings, provider
memory/eviction alerts.

Actions:

1. Restore the managed Redis or fix DNS/TLS/maxclients. Do not flush it.
2. Expect duplicate mutations and weaker rate limiting during the outage.
3. Treat security-sensitive changes (ban, password change, role change) made
   during the outage as potentially not propagated; re-check after recovery.
4. Pods stay ready (`degraded`) while the database is healthy. Restoring Redis
   is the primary fix; a memory-cache restart is an emergency, single-replica
   degradation only (production also requires
   `CACHE_ALLOW_MEMORY_IN_PRODUCTION=true`).
5. After recovery, verify login, a rate-limited endpoint, and an idempotent
   mutation.

### SMTP is failing

Symptoms: users do not receive verification/reset email; `email.send` or
`email.send.encrypted` jobs retry; `jobs_dead_total` rises; relay errors in
logs. Requests that successfully enqueue a job are not failed by an SMTP
outage; delivery waits and retries.

Actions:

1. Check `SMTP_HOST`, `SMTP_PORT` (normally 587), STARTTLS support,
   credentials, `EMAIL_FROM_ADDRESS`, and relay rate limits.
2. Confirm `SMTP_REQUIRE_TLS=true` in production; do not disable TLS to work
   around a relay problem.
3. Send a test message through the relay. The API and database can be healthy
   while email is broken.
4. If jobs exhausted attempts and are `dead`, their payload is cleared.
   Re-trigger the user flow; do not requeue the dead row.
5. Expect duplicate emails after retries; delivery is at-least-once.

### Job backlog or stuck queue

Symptoms: old `queued`/`running` rows, queue age rising, no claim activity,
`jobs_claim_errors_total` increasing, slow requests.

Actions:

1. Confirm workers are running. They are in-process: scaling the API to zero
   stops processing, and a crashed pod stops its worker.
2. Check PostgreSQL health and `jobs` locks; claim failures back off to 30s.
3. Look for an unregistered kind, a poison payload, or a handler exceeding its
   10m timeout. Unknown kinds retry until `max_attempts` before dying.
4. Scale API replicas within the DB connection budget to add workers; do not
   exceed `DB_MAX_CONNS * replicas`.
5. Let lease expiry reclaim crashed work. Manual lock clearing is a last resort
   and accepts duplicate execution.
6. Do not bulk-requeue `dead` rows: payloads are cleared. Fix the handler and
   re-trigger the business flow.

### Webhook deliveries are failing or delayed

Symptoms: `webhook_deliveries` pending/dead counts rising, `last_error`
showing timeouts/5xx, receiver reports duplicate or missing events.

Actions:

1. Check the endpoint status, URL, event filters, and attempt budget. Disabled
   or soft-deleted endpoints are not matched for new events.
2. Inspect `webhook_delivery_attempts` for the status code and duration. A
   timeout usually means the receiver is slow or unreachable; confirm the
   configured `timeoutMs`.
3. Remember `3xx` is permanent by design and redirects are never followed.
   Fix the endpoint URL rather than expecting a retry.
4. `408`, `425`, `429`, and `5xx` retry with backoff/`Retry-After`; a sustained
   `429` means the receiver is throttling and the delivery may eventually die.
5. Check that the worker is running (it is in-process with the API) and that
   `jobs` has `webhook.delivery` rows. The delivery claim lease recovers a
   crashed attempt; if no job remains, replay the delivery.
6. A dead delivery retains its payload and secret snapshot, so replay is
   possible after fixing the receiver. Replay uses the current URL/secret.
7. For inbound failures, use the receiver troubleshooting checklist in
   [Webhooks](webhooks.md) or section 5 above; check clock skew and signature
   canonicalization before rotating secrets.

### Migration failed or schema is dirty

Actions:

1. Stop the rollout and read the migration Job logs and database logs.
2. Inspect `schema_migrations`; a dirty flag means the previous attempt did not
   finish cleanly.
3. Repair forward with a reviewed migration or restore the verified backup/PITR
   snapshot if data is at risk. Do not reset history or edit a shipped
   migration.
4. Re-run the Job only after the dirty state is understood and repaired.
5. Start the API only after `cmd/migrate` verifies the required schema.

### After the incident

- Record start/end, affected routes/jobs, request IDs, metrics, and the exact
  mitigation.
- Add or tune an alert for the failure mode; prefer symptom-based alerts over
  raw queue depth.
- Update this runbook and the module README if a module caused the incident.
- Add a regression test for any code or migration fix.
