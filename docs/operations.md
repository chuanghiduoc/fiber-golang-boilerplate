# Operations Runbook

Runbook for the deployed API: health, metrics, logs, jobs, idempotency,
migrations, Kubernetes, and incident response. It describes the behavior in
this repository; replace every `<placeholder>` and adapt thresholds to your
service levels.

Related documents:

- [Kubernetes deployment](../deploy/k8s/README.md) — manifests, migration Job
  order, placeholders, rollback.
- [Architecture](architecture.md) — modules, dependency rules, data ownership.
- [Module Template](module-template.md) — adding a module, routes, permissions,
  tables, and contracts.

Never put real secrets in this file, a ticket, a log line, or an error message.

## 1. Health and readiness

| Endpoint | Purpose | Behavior |
| --- | --- | --- |
| `GET /healthz` | Liveness | Always `200` with `{"status":"up"}` while the process runs. Does not check dependencies. |
| `GET /readyz` | Readiness | Checks PostgreSQL and cache concurrently (2s each). `200` only when both are up; otherwise `503` with `status:"degraded"`. |
| `GET /health` | Readiness alias | Same handler and response as `/readyz`. |
| `GET /metrics` | Prometheus | Bearer token required when `METRICS_AUTH_TOKEN` is set. Production config requires a token of at least 16 characters. |
| `GET /swagger` | API docs | Non-production only. |

Readiness details include `database`, `cache`, and, when the database is up,
`db_total_conns` and `db_idle_conns`:

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

Important trade-off: because `/readyz` includes the cache, a Redis outage marks
every replica unready even if PostgreSQL is healthy. If the product must serve
degraded reads during a cache outage, that is a deliberate readiness-design
change, not something to improvise during an incident.

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
| `jobs_claim_errors_total` | — | Failed claim attempts; the worker retries with backoff capped at 30s. |
| `jobs_purged_total` | — | Terminal job rows deleted by retention. |
| `audit_write_failures_total` | — | Audit events that could not be recorded. |

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

## 3. Logs and request IDs

`APP_ENV=production` and `staging` use JSON `slog` output; other environments
use text. `LOG_LEVEL` is `debug`, `info`, `warn`, or `error` (default `info`).
Logs go to stdout; collect them with the platform's normal log pipeline.

The request logger emits one record per request with:

`method`, `path`, `status`, `latency`, `ip`, `request_id`, `user_id`,
`content_length`, `user_agent`, and `query`.

- `status` is the effective status after the Fiber error handler; a handler
  returning an error is not logged as `200`.
- 4xx requests log at `WARN`; 5xx log at `ERROR`; the rest log at `INFO`.
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

Audit events carry the same request ID. The generic audit middleware records
successful mutating requests; idempotent replays are skipped so a replay does
not duplicate the trail. See [Module Template](module-template.md#5-audit) for
explicit domain events.

## 4. Background jobs

`internal/platform/jobs` is a PostgreSQL-backed durable queue. The API hosts
the worker in-process; every replica runs a worker with concurrency 4. Claims
use `FOR UPDATE SKIP LOCKED`, so multiple replicas are safe.

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
- On SIGINT/SIGTERM the worker stops claiming, cancels running handlers, and
  the process waits up to 10s before shutting down HTTP. A non-cooperative
  handler can exceed that window.

Delivery is **at-least-once**, never exactly-once. Duplicate emails and
duplicate side effects must be expected.

### Retention and payload clearing

Workers purge `succeeded` and `dead` rows older than 30 days, hourly by
default; the first pass runs when a worker starts. Queued and running rows are
never purged. Each pass is bounded to 5s and deletes at most 1,000 rows per
batch, so a large backlog cannot hold one transaction open.

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
-- Backlog by kind and state
SELECT kind, status, count(*), min(run_at), max(updated_at)
FROM jobs
WHERE status IN ('queued', 'running')
GROUP BY kind, status
ORDER BY count(*) DESC;

-- Recent failures and dead letters
SELECT id, kind, status, attempts, max_attempts, last_error, updated_at
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

## 5. Idempotency and Redis

Idempotent mutations are opt-in with `Idempotency-Key` (1–128 chars of
`[A-Za-z0-9._:-]`). The cache stores:

| Key | TTL | Purpose |
| --- | --- | --- |
| `idempotency:<method>:<path>:<identity>:<key>:lock` | 2 minutes | Owner-token lock for the in-flight request. |
| `idempotency:<method>:<path>:<identity>:<key>:response` | 24 hours | Cached 2xx status, body, and selected headers for replay. |

`identity` is the authenticated user ID, or the client IP for public routes.
Keys are scoped by method, path, and identity, so the same client key on
another endpoint or user does not collide.

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
- Memory cache is per process. It is not suitable for multiple replicas or for
  replay/lock guarantees across restarts. Use `CACHE_DRIVER=redis` in
  production.
- Rate-limit cache errors also fail open. Login lockouts are a separate,
  fail-closed mechanism and are not relaxed by a cache error.

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

- `/readyz` reports `cache: down` and returns `503`; Kubernetes removes pods
  from the Service even though PostgreSQL may be healthy.
- Session validation falls back to PostgreSQL, but cache invalidation writes
  can fail. Bans, password changes, and role changes made during the outage may
  not propagate to every replica immediately. Re-check security-sensitive
  accounts after recovery.
- Idempotency and rate limits fail open. Expect duplicate mutations and weaker
  abuse protection until the cache returns.
- Do not switch `CACHE_DRIVER` in place; environment changes require a restart.
  Moving to memory is an emergency, single-replica-only degradation and loses
  shared state. Revert as soon as Redis is healthy.

## 6. Migrations and legacy upgrade

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
- Once a database is deployed, never edit `000001`. Add a numbered migration
  with only the new objects.
- A database that applied the old `000001`–`000003` history must keep that
  history. Restore the old source files from git history (golang-migrate needs
  the recorded version to exist), add `000004` or later, and run
  `cmd/migrate`. Never point an old database at the consolidated `000001` and
  never reset `schema_migrations` to force it through.
- If a fresh schema was created out-of-band and already matches the baseline,
  verify the objects and baseline the version (for example `force 1`) instead
  of applying the file over populated tables.

### Multi-tenant foundation upgrade

The baseline now creates `tenants`, seeds tenant id 1 (`default`), adds
`tenant_id BIGINT NOT NULL DEFAULT 1 REFERENCES tenants(id)` to users, files,
refresh/reset/verification tokens, jobs, and audit events, adds tenant-leading
indexes, and scopes the last-admin trigger per tenant. A database deployed
before this change does **not** receive those objects from the edited baseline.
Create the next numbered migration above its recorded version (or apply
reviewed DDL manually) that:

1. creates `tenants` and inserts `('default','Default')` first so it gets id 1;
2. adds/backfills the `tenant_id` columns (default 1) and foreign keys;
3. rebuilds user uniqueness as `(tenant_id, lower(email))` and
   `(tenant_id, google_id)` for active rows, and adds the tenant-leading
   indexes;
4. replaces `protect_last_admin()` with the per-tenant body.

Keep tenant id 1 as the compatibility anchor unless you also change the column
defaults and backfill every row. After the upgrade, run `cmd/migrate`; it
verifies the tenant table/columns/indexes and the default tenant row.

`TENANT_DEFAULT_SLUG` (default `default`) controls which tenant header-less
requests resolve to; `TENANT_REQUIRE_HEADER=true` rejects requests that omit
`X-Tenant-Slug`. Roll out the latter only after all clients send the header.
This foundation does not scope existing queries yet; see the README scope note.

`cmd/migrate` checks for a dirty migration state before running, and after
migrating verifies required tables, columns, and unique indexes. A legacy
partial schema fails with an actionable error instead of silently serving a
broken database.

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

## 7. Kubernetes deployment notes

Follow [deploy/k8s/README.md](../deploy/k8s/README.md) for the full commands.
The operational invariants:

1. Apply namespace, ConfigMap, and Secret first.
2. Delete the old migration Job, apply `migration-job.yaml`, wait for
   `condition=complete`, and read its logs.
3. Only then apply the app kustomization. `kubectl apply -k` has no cross-
   resource ordering guarantee, and the migration Job is deliberately excluded
   from the kustomization.
4. `kubectl apply -k deploy/k8s` does not include the Job or
   `secret.example.yaml`.

Other caveats:

- **HPA owns replicas.** The Deployment intentionally has no `spec.replicas`.
  Do not add it back or the HPA and Deployment controller will fight. The HPA
  needs metrics-server; `<unknown>` targets mean metrics are unavailable.
- **PDB:** `minAvailable: 1`; with two replicas, node drains proceed one pod at
  a time.
- **NetworkPolicy probes are CNI-dependent.** Some CNIs allow kubelet probes
  regardless of policy; others drop them. If probes fail, use the commented
  node/node-pod CIDR rule, never `0.0.0.0/0`. A Prometheus scrape also needs an
  ingress rule for the monitoring namespace; it is commented in the manifest.
- **Config changes are not hot-reloaded.** `envFrom` is read at container
  start. After editing the ConfigMap or Secret, run
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
  replicas.
- **Redis:** `CACHE_DRIVER=redis` and `REDIS_URL` (use `rediss://` when
  available) are required for shared rate limits, session invalidation, and
  idempotency across replicas. Memory cache is single-replica only.
- **Database connections:** `DB_MAX_CONNS` is per pod. Keep
  `DB_MAX_CONNS * maxReplicas` below the database limit; the template uses 10
  with HPA max 10, for at most 100 connections.
- **Migration Job:** its image tag is literal and outside the kustomization's
  image transformer. Keep it in sync with the Deployment's release tag.
- **Secrets:** never commit a rendered Secret. Use External Secrets, Sealed
  Secrets, SOPS, or the platform secret manager.

## 8. Incident checklist

### First 5 minutes

1. Confirm scope: `/readyz`, pod status, recent rollout/migration, and the
   affected route or job kind.
2. Correlate with `X-Request-Id` in logs, problem responses, and audit rows.
3. Check the four golden signals plus `jobs_dead_total`,
   `jobs_claim_errors_total`, and `audit_write_failures_total`.
4. Prefer reversible mitigations: roll back the Deployment, restore a
   dependency, or scale capacity. Do not flush Redis, rotate `JWT_SECRET`,
   delete jobs, or force a migration version as a first response.
5. Record the timeline and evidence while it is fresh.

### PostgreSQL is down or saturated

Symptoms: `/readyz` shows `database: down` (503), 5xx responses, session
validation failures, `jobs: claim failed` warnings, rising
`jobs_claim_errors_total`.

Actions:

1. Check DNS, network policy, TLS mode, credentials, and provider status.
2. Check connection saturation: `db_total_conns` vs `DB_MAX_CONNS` and the
   database's own limit. Do not raise the pool beyond capacity.
3. If failover is in progress, keep pods alive; liveness does not depend on the
   database. Readiness will recover automatically.
4. After recovery, confirm `/readyz`, watch job backlog drain, and check for
   duplicate side effects from retried requests.

### Redis is down or degraded

Symptoms: `/readyz` shows `cache: down` (503), rate limits stop rejecting,
idempotency replay/locks fail open, session invalidation warnings, provider
memory/eviction alerts.

Actions:

1. Restore the managed Redis or fix DNS/TLS/maxclients. Do not flush it.
2. Expect duplicate mutations and weaker rate limiting during the outage.
3. Treat security-sensitive changes (ban, password change, role change) made
   during the outage as potentially not propagated; re-check after recovery.
4. If readiness removes all pods, service is unavailable even though the
   database is healthy. Restoring Redis is the primary fix; a memory-cache
   restart is an emergency, single-replica degradation only.
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
