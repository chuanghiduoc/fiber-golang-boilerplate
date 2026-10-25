# 0002. Module layering: transport-agnostic application layer

- Status: Accepted
- Date: 2026-10-25

## Context

Modules are isolated from each other (test/arch), but not internally layered:
service packages import sqlc rows, `pgx`, and transport DTOs, and
`pkg/apperror` depended on Fiber, so business code could not run behind
another transport (gRPC/Connect, CLI, jobs) and could not grow into DDD/CQRS
without rewrites. Sources: [Hexagonal architecture](https://alistair.cockburn.us/hexagonal-architecture/),
[Three Dots Labs: DDD + CQRS + Clean Architecture](https://threedots.tech/post/ddd-cqrs-clean-architecture-combined/),
[Go module layout](https://go.dev/doc/modules/layout).

## Decision

One module layout serves both simple CRUD and DDD/CQRS modules:

| Directory | Role | May import |
| --- | --- | --- |
| `module.go`, `build.go` | composition | everything in the module |
| `catalog/` | permissions, error codes, flags, locales | platform |
| `domain/` (optional) | aggregates, value objects, invariants, domain events | stdlib, catalog |
| `service/` (+ `ports.go`) | use cases; `command_*.go` / `query_*.go` when CQRS helps | domain, contracts, platform cores |
| `handler/` + `dto/` | HTTP driving adapter | service, transport helpers |
| `worker/` (optional) | job/event driving adapter | service |
| `repository/` + `queries/` + `sqlc/` | Postgres driven adapter, maps rows to service types | sqlc, pgx |
| `adapter/` | implements contracts for other modules | service |

`service/` must not import sqlc, pgx, Fiber, or `dto/`; `handler/` must not
import repositories, sqlc, or pgx. Platform packages expose a framework-free
core with Fiber adapters separated, and authorization has one path
(`authz.Authorize`) that fails closed unless a subject or an explicit system
context is present.

## Consequences

- Enforced by a layering test in `test/arch` with a ratchet allowlist: new
  violations fail; fixed entries must be removed from the allowlist.
- Existing modules convert incrementally (files first, identity last).
- The generator (`cmd/gen`) emits this layout, including the row-to-type
  mapping, so the extra mapping layer costs nothing for CRUD modules.
