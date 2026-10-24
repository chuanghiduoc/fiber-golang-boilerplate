package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// metadataLocalKey is the Fiber locals key modules use to enrich the generic
// audit event with domain-specific, non-secret details.
const metadataLocalKey = "audit_metadata"

// AddMetadata merges domain-specific metadata into the audit event that the
// audit middleware records after a successful mutation. It must be called from
// a handler (or earlier) inside the request. Reserved keys status and path are
// overwritten by the middleware.
func AddMetadata(c fiber.Ctx, metadata map[string]any) {
	if len(metadata) == 0 {
		return
	}
	existing := fiber.Locals[map[string]any](c, metadataLocalKey)
	merged := make(map[string]any, len(existing)+len(metadata))
	for key, value := range existing {
		merged[key] = value
	}
	for key, value := range metadata {
		merged[key] = value
	}
	fiber.Locals[map[string]any](c, metadataLocalKey, merged)
}

// Middleware records successful mutating requests (POST/PUT/PATCH/DELETE) with
// the authenticated actor and route template. It is intentionally generic so
// every module gets an audit trail without adding boilerplate to each handler.
func Middleware(recorder Recorder) fiber.Handler {
	if recorder == nil {
		recorder = Noop{}
	}

	return func(c fiber.Ctx) error {
		if err := c.Next(); err != nil {
			return err
		}
		switch c.Method() {
		case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete:
		default:
			return nil
		}
		status := c.Response().StatusCode()
		if status < 200 || status >= 300 || c.Route().Path == "" {
			return nil
		}
		// Idempotency replay is a read of an earlier mutation's result, not a
		// new mutation; recording it would duplicate the audit trail.
		if fiber.Locals[bool](c, "idempotency_replayed") {
			return nil
		}

		actorID := fiber.Locals[int64](c, "user_id")
		tenantID := tenant.ID(c)
		if tenantID == 0 {
			tenantID = tenant.EffectiveID(c.Context())
		}
		metadata := make(map[string]any, 2)
		for key, value := range fiber.Locals[map[string]any](c, metadataLocalKey) {
			metadata[key] = value
		}
		metadata["status"] = status
		metadata["path"] = c.Path()

		resourceID := c.Params("id")
		if resourceID == "" {
			resourceID = c.Params("key")
		}
		event := Event{
			TenantID:   tenantID,
			ActorID:    actorID,
			Action:     c.Method() + " " + c.Route().Path,
			Resource:   c.Route().Path,
			ResourceID: resourceID,
			Metadata:   metadata,
			IP:         c.IP(),
			UserAgent:  c.Get("User-Agent"),
			RequestID:  fiber.Locals[string](c, "request_id"),
			CreatedAt:  time.Now().UTC(),
		}

		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), 2*time.Second)
		defer cancel()
		if err := recorder.Record(ctx, event); err != nil {
			metrics.AuditWriteFailuresTotal.Inc()
			slog.Warn("failed to record audit event",
				slog.String("action", event.Action),
				slog.Any("error", err),
			)
		}
		return nil
	}
}
