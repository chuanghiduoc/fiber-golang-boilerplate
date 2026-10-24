# OpenTelemetry tracing

The API can emit W3C Trace Context spans for HTTP requests, PostgreSQL
queries, and durable jobs. Tracing is **disabled by default**. When disabled,
the process uses a no-op provider: no span is recorded and no collector
connection is opened.

## Quick start

1. Start the local collector profile:

   ```bash
   docker compose --profile otel up --build
   ```

   The collector receives OTLP/gRPC on `127.0.0.1:4317` and OTLP/HTTP on
   `127.0.0.1:4318`, and prints batches to its log. The collector is an
   optional Compose profile, so a normal `docker compose up` never starts it
   and the app has no `depends_on` on it.

2. Enable tracing for the app:

   ```bash
   OTEL_ENABLED=true \
   OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 \
   go run ./cmd/api
   ```

   `OTEL_TRACES_EXPORTER=none` is also accepted: spans are sampled and
   propagated (so logs still carry `trace_id`/`span_id`) but not exported.
   This is useful when debugging sampling without a collector.

3. Send a request and look for a `trace_id` in the request log:

   ```bash
   curl -fsS http://127.0.0.1:8080/healthz   # excluded, no span
   curl -fsS http://127.0.0.1:8080/api/v1/auth/me
   ```

## Configuration

All settings are optional and read at startup through `config.Load`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `OTEL_ENABLED` | `false` | Master switch. Invalid tracing values are ignored while false. |
| `OTEL_TRACES_EXPORTER` | `otlp` | `otlp` exports over OTLP/gRPC; `none` samples without exporting. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | Collector `host:port` (no scheme). |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | Only `grpc` is supported. |
| `OTEL_EXPORTER_OTLP_INSECURE` | `true` | Disable TLS to the collector. Set `false` for a TLS collector. |
| `OTEL_SERVICE_NAME` | `fiber-golang-boilerplate` | Resource `service.name`. |
| `OTEL_SERVICE_NAMESPACE` | empty | Resource `service.namespace`. |
| `OTEL_SERVICE_VERSION` | `dev` | Resource `service.version`; set the release version in CD. |
| `OTEL_DEPLOYMENT_ENVIRONMENT` | `local` | Resource `deployment.environment.name`. |
| `OTEL_SERVICE_INSTANCE_ID` | hostname | Resource `service.instance.id`. |
| `OTEL_TRACES_SAMPLER` | `parentbased_traceidratio` | `parentbased_traceidratio`, `parentbased_always_on`, `parentbased_always_off`, `traceidratio`, `always_on`, or `always_off`. |
| `OTEL_TRACES_SAMPLER_ARG` | `1.0` | Ratio in `[0,1]` for the ratio samplers. |
| `OTEL_TRACING_EXCLUDE_PATHS` | `/healthz,/readyz,/health,/metrics` | Exact request paths that never create spans. |
| `OTEL_INSTRUMENT_DB` | `true` | Emit sanitized pgx client spans under a recording parent. |
| `OTEL_TRUST_INCOMING_TRACE` | `false` | Honor an inbound W3C `traceparent`. With `APP_TRUST_PROXY=true` it is honored only from a peer in `APP_TRUSTED_PROXIES`. Off by default so a client cannot force sampling. |
| `OTEL_BATCH_TIMEOUT_SECS` | `5` | Maximum delay before a batch is exported. |
| `OTEL_EXPORT_TIMEOUT_SECS` | `30` | Per-export timeout. |
| `OTEL_SHUTDOWN_TIMEOUT_SECS` | `10` | Bounded flush on process shutdown. |
| `OTEL_BATCH_MAX_QUEUE_SIZE` | `2048` | Spans buffered in memory; new spans are dropped when full. |
| `OTEL_BATCH_MAX_EXPORT_BATCH_SIZE` | `512` | Spans per export request; must not exceed the queue size. |

Validation runs only when `OTEL_ENABLED=true`. A malformed endpoint, sampler,
or batch setting fails startup with an actionable configuration error. An
**unreachable collector never fails startup**: the OTLP/gRPC client connects
lazily and export failures only drop spans.

## What is recorded

### HTTP server span

Created for every request except the excluded probe paths, after `RequestID`
and before `Metrics`/`Logger`/`Recovery`:

- name: `METHOD /route/template` (for unmatched routes: just `METHOD`)
- kind: server
- `http.request.method`, `http.route` (route template, never the raw path or
  query), `http.response.status_code`
- `app.request_id`, `enduser.id`, `tenant.id`
- `app.error_code` for `apperror.AppError` values (for example
  `internal_error`, `not_found`)
- 5xx: status `Error` and a bounded `error.type`; 4xx stays `Unset`
- inbound W3C `traceparent` is extracted only when
  `OTEL_TRUST_INCOMING_TRACE=true` (and, behind `APP_TRUST_PROXY`, only from a
  trusted proxy). Otherwise it is ignored and every request starts a new root
  trace sampled by the local ratio: with a parent-based sampler an untrusted
  client's sampled flag would otherwise force 100% sampling and export cost.

### PostgreSQL client span

Each `Query`/`QueryRow`/`Exec` that runs while a recording span is active gets a
client span:

- name: `db SELECT` / `db INSERT` / ...
- `db.system.name=postgresql`, `db.operation.name`
- failures: `db.response.status_code` with the PostgreSQL SQLSTATE and a
  bounded `error.type`
- no span is created when the parent is not recording, so background work
  without a trace does not create orphan spans
- `pgx.ErrNoRows` is a normal miss, not an error

### Job consumer span

`Queue.Enqueue`/`EnqueueTx` persist the active W3C `traceparent` in
`jobs.trace_parent`. The worker starts each attempt as a **new root**
`job.process` consumer span and adds a link to the enqueue span, then installs
the tenant context before running the handler:

- `job.id`, `job.kind`, `job.tenant_id`, `job.attempt`, `job.max_attempts`
- `job.outcome` (`succeeded`, `retried`, `dead`, `released` on shutdown), `job.dead_letter`,
  `job.retry_at` when retried
- failures: status `Error` and a bounded `error.type`

The handler's `context.Context` carries the span, so sanitized DB spans nest
under `job.process`. Retries create a fresh root, which keeps each attempt
independently inspectable.

## Privacy rules

Spans are designed to be safe to export to a third-party backend:

- No request or response bodies, headers, query strings, emails, tokens,
  uploaded file names, SQL text, bind parameters, or raw error messages.
- `error.type` is a bounded class (Go type name or `http_<status>`), never
  `err.Error()`. `error.description` is never set.
- Errors are not recorded as span events or exceptions.
- The request path is recorded only as the matched route template. Unmatched
  requests record no route.
- Trace IDs are safe correlation identifiers and may appear in logs.

## Kubernetes

`deploy/k8s/prereqs/configmap.yaml` contains a disabled tracing block and the
NetworkPolicy already allows egress to a collector on TCP 4317 (narrow the
CIDR to your collector namespace/service range). Enable it per environment:

```yaml
OTEL_ENABLED: "true"
OTEL_EXPORTER_OTLP_ENDPOINT: "otel-collector.observability.svc.cluster.local:4317"
OTEL_SERVICE_VERSION: "<release-tag>"
OTEL_DEPLOYMENT_ENVIRONMENT: "production"
OTEL_TRACES_SAMPLER_ARG: "0.1"
```

If the collector uses TLS, set `OTEL_EXPORTER_OTLP_INSECURE: "false"` and make
sure the pod trusts the collector CA. No readiness or liveness probe depends on
the collector.

## Schema upgrade note

`jobs.trace_parent` is part of the consolidated `migrations/000001` baseline for
fresh installs. The project has no deployed database yet, so no new migration
file was added. If you already applied version 1 to a database, add the column
once before deploying this build:

```sql
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS trace_parent TEXT;
```

`cmd/migrate` verifies the column exists and fails with an actionable message
otherwise. Existing rows have `NULL`, which means the worker creates a root
span without a link.

## Operations

- Collector down: spans queue up to `OTEL_BATCH_MAX_QUEUE_SIZE`, then drop.
  The API stays ready. Check the collector endpoint and network policy; do not
  restart the API expecting spans to recover.
- Missing traces: verify `OTEL_ENABLED=true`, the endpoint, the sampler ratio,
  and that the route is not excluded. With `OTEL_TRUST_INCOMING_TRACE=true`,
  inbound sampling follows the trusted parent; a `traceparent` with the sampled
  flag off keeps the trace unsampled. Upstream traces not joining is expected
  while the flag is off.
- Shutdown: the provider flushes last, after HTTP and the job worker stop. A
  failed flush logs a warning but never changes the process exit code.
- Logs: request records include `trace_id` and `span_id` when a recording span
  exists. Search logs by `request_id`, then pivot to the trace by `trace_id`.
