# Documentation

- [Architecture](architecture.md) — module map, dependency rules, request flow,
  data ownership, jobs/outbox, migration policy.
- [API Design System](api-design.md) — URL/versioning, JSON conventions,
  success/error shapes, pagination, auth, idempotency, rate limits, status
  codes, compatibility rules.
- [Module Template](module-template.md) — canonical module layout, copy-paste
  skeleton, route/permission/audit/idempotency wiring, table ownership,
  cross-module contracts, migrations, tests, and the add-a-module checklist.
- [Operations](operations.md) — health/readiness, metrics, logs and request
  IDs, jobs, idempotency and Redis, migrations, Kubernetes caveats, and
  incident checklists.
- [Swagger UI](../docs/swagger.json) — generated OpenAPI (served at `/swagger`
  outside production).

Operational deployment and tooling:

- [Kubernetes](../deploy/k8s/README.md) — manifests, migration Job, release
  workflow, external DB/Redis, storage choices.
- [New module scaffold](../scripts/new-module.sh) — `scripts/new-module.sh
  <name>` creates the module layout described in the module template.
