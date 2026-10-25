# Documentation

| Document | Covers |
|---|---|
| [Architecture Decisions](adr/README.md) | ADRs: why the layering, RBAC, tenancy, migration, API, webhook, MFA, and event decisions were made |
| [Architecture](architecture.md) | Module map and dependency rules, request lifecycle, cross-module communication, tenant model, authorization, transactions/outbox, migration policy |
| [Data Model](data-model.md) | ER diagrams of the baseline schema, table ownership, keys, and FK delete actions |
| [API Design System](api-design.md) | URLs/versioning, JSON conventions, success/error (RFC 9457) shapes, pagination, JWT vs API-key auth, tenants, headers, rate limits, idempotency, compatibility rules |
| [Module Template](module-template.md) | Canonical module layout, skeleton, route/permission/audit/idempotency wiring, table ownership, contracts, migrations, tests, add-a-module checklist |
| [Operations](operations.md) | Health/readiness, metrics, logs and request IDs, jobs, scheduler, shutdown, idempotency and Redis, migrations, Kubernetes caveats, incident checklists |
| [Feature Flags](feature-flags.md) | Compile-time catalog, precedence, environment force-off, database defaults and tenant/user targets, cache staleness, admin API, metrics |
| [Webhooks](webhooks.md) | Outbound dispatch, HMAC signing and verification, SSRF-safe delivery, retries, replay, inbound sources, secrets, configuration |
| [Tracing](tracing.md) | OpenTelemetry configuration, span privacy rules, collector, job trace links |
| [Swagger](swagger.json) | Generated OpenAPI (served at `/swagger` outside production) |

Deployment and tooling:

| Link | Covers |
|---|---|
| [Kubernetes](../deploy/k8s/README.md) | Manifests, staging/production overlays, GitHub Actions CD, migration Job, release workflow, external DB/Redis, storage choices |
| [New module scaffold](../scripts/new-module.sh) | `scripts/new-module.sh <name>` creates the module layout from the template and verifies it with `go build` |
