# 0009. Webhooks follow the Standard Webhooks spec

- Status: Accepted
- Date: 2026-10-25

## Context

Webhooks are signed with HMAC-SHA256 over `<id>.<timestamp>.<event>.<body>`
and sent with `X-Webhook-*` headers and `v1=<hex>` signatures. Receivers must
write custom verification code. [Standard Webhooks](https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md)
defines `webhook-id`, `webhook-timestamp`, and `webhook-signature: v1,<base64>`
over `<id>.<timestamp>.<body>`, with official verification libraries.

## Decision

Adopt Standard Webhooks for outbound and inbound webhooks. The event type moves
into the signed payload (versioned envelope, ADR 0011) instead of the signed
string. Multiple signatures remain supported during secret rotation.

## Consequences

Breaking change for existing receivers; shipped together with the event
envelope change and documented in `docs/webhooks.md` and the CHANGELOG.
