# 0011. Versioned event envelope; webhooks subscribe to domain events

- Status: Accepted
- Date: 2026-10-25

## Context

The in-process event bus (`internal/platform/events`) had no production
publishers or subscribers; producers called the webhook dispatcher directly,
and webhook payloads reused REST DTOs, so changing an API response silently
changed a public webhook contract. Events had no version. Sources:
[CloudEvents](https://github.com/cloudevents/spec/blob/main/cloudevents/spec.md),
[Transactional outbox](https://microservices.io/patterns/data/transactional-outbox.html).

## Decision

- One envelope: `id` (UUIDv7), `type`, `dataVersion`, `occurredAt`, `tenantId`,
  `actor`, `data`; maps 1:1 onto CloudEvents attributes.
- Each event has its own payload struct in `internal/contracts`; never a REST
  DTO. Evolution is additive; a breaking change gets a new `dataVersion` or type.
- Producers publish with `events.PublishTx` inside their transaction; webhooks,
  notifications, and other modules subscribe durably. A future broker relay
  (e.g. NATS JetStream) reads the same outbox without touching producers.

## Consequences

Webhook payloads change shape (shipped with ADR 0009). Durable handlers stay
at-least-once and idempotent.
