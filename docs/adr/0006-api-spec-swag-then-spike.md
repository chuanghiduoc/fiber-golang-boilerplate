# 0006. Keep swag; spike OpenAPI-first before switching

- Status: Accepted
- Date: 2026-10-25

## Context

The API spec is generated from annotations by swag (Swagger 2.0). Spec-first
generation ([oapi-codegen](https://github.com/oapi-codegen/oapi-codegen),
strict server) gives typed adapters and SDKs, but its generated router may not
preserve the per-route middleware chain that `test/arch` enforces
(limiter → authz → idempotency → audit).

## Decision

Keep swag. Convert the generated spec to OpenAPI 3 in CI and detect breaking
changes with [oasdiff](https://github.com/oasdiff/oasdiff). Run a spike of
oapi-codegen (types + strict handlers, routes still registered by hand) on one
new module, then decide in a follow-up ADR.

## Consequences

- Breaking API changes fail CI against `main`.
- No mass rewrite of handlers until the spike proves the middleware chain and
  RFC 9457 errors are preserved.
