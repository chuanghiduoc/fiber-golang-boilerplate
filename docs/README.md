# Documentation

- [Architecture](architecture.md) — module map, dependency rules, request flow,
  data ownership, jobs/outbox, migration policy.
- [API Design System](api-design.md) — URL/versioning, JSON conventions,
  success/error shapes, pagination, auth, idempotency, rate limits, status
  codes, compatibility rules.
- [Swagger UI](../docs/swagger.json) — generated OpenAPI (served at `/swagger`
  outside production).

Operational deployment:

- [Kubernetes](../deploy/k8s/README.md) — manifests, migration Job, release
  workflow, external DB/Redis, storage choices.
