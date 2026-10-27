package audithttp

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// AddMetadata merges domain-specific metadata into the audit event that the
// audit middleware records after a successful mutation. It must be called from
// a handler (or earlier) inside the request. Reserved keys status and path are
// overwritten by the middleware.
func AddMetadata(c fiber.Ctx, metadata map[string]any) {
	if len(metadata) == 0 {
		return
	}
	existing := fiber.Locals[map[string]any](c, audit.MetadataLocalKey)
	merged := make(map[string]any, len(existing)+len(metadata))
	for key, value := range existing {
		merged[key] = value
	}
	for key, value := range metadata {
		merged[key] = value
	}
	fiber.Locals[map[string]any](c, audit.MetadataLocalKey, merged)
}

// Middleware records successful mutating requests (POST/PUT/PATCH/DELETE) with
// the authenticated actor and route template. It is the process-wide fallback
// for routes that are not individually audited.
//
// Routes protected by [Guard] set the audit_guarded local and are skipped here
// so one mutation never produces two independent audit records. Idempotent
// replays and failed requests are also skipped; Guard owns the outcome trail
// for Tier 2 routes.
func Middleware(recorder audit.Recorder) fiber.Handler {
	if recorder == nil {
		recorder = audit.Noop{}
	}

	return func(c fiber.Ctx) error {
		// Install the request's RequestInfo up front so JWT auth enriches it
		// (WithActor/WithRequestMeta reuse an existing pointer) and a Tier 1
		// service's RecordInTx can mark it as already audited.
		info := audit.RequestInfoFromContext(c.Context())
		if info == nil {
			info = &audit.RequestInfo{RequestID: requestctx.RequestID(c.Context())}
			c.SetContext(audit.WithRequestInfo(c.Context(), info))
		}

		if err := c.Next(); err != nil {
			return err
		}
		if !needsFallbackRecord(c, info) {
			return nil
		}
		recordFallback(c, recorder, fallbackEvent(c))
		return nil
	}
}

// needsFallbackRecord reports whether the request is a successful mutation
// that no other audit path has recorded.
func needsFallbackRecord(c fiber.Ctx, info *audit.RequestInfo) bool {
	switch {
	case fiber.Locals[bool](c, auditGuardedLocal):
		// Guard already wrote and completed an intent row.
		return false
	case info.ServiceRecorded > 0:
		// A Tier 1 service wrote this mutation's row inside its transaction; a
		// second generic row would duplicate the trail.
		return false
	case fiber.Locals[bool](c, idempotencyReplayedLocal):
		// A replay is a read of an earlier mutation's result, not a mutation.
		return false
	}
	switch c.Method() {
	case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete:
	default:
		return false
	}
	status := c.Response().StatusCode()
	return status >= 200 && status < 300 && c.Route().Path != ""
}

// fallbackEvent describes the completed mutation from the request: actor,
// route template, target id, and handler-supplied metadata. Never put secrets
// in that metadata: it is persisted after sanitization.
func fallbackEvent(c fiber.Ctx) audit.Event {
	actorID := requestctx.ActorID(c.Context())
	role := requestRole(c)
	tenantID := tenanthttp.ID(c)
	if tenantID == uuid.Nil {
		tenantID = tenant.IDFromContext(c.Context())
	}
	status := c.Response().StatusCode()

	extra := fiber.Locals[map[string]any](c, audit.MetadataLocalKey)
	metadata := make(map[string]any, len(extra)+2)
	for key, value := range extra {
		metadata[key] = value
	}
	metadata["status"] = status
	metadata["path"] = c.Path()

	now := time.Now().UTC()
	event := audit.Event{
		TenantID:    tenantID,
		ActorID:     actorID,
		ActorType:   audit.ActorTypeForRole(role),
		ActorRole:   role,
		Action:      c.Method() + " " + c.Route().Path,
		Resource:    c.Route().Path,
		ResourceID:  routeResourceID(c),
		Metadata:    metadata,
		IP:          c.IP(),
		UserAgent:   c.Get("User-Agent"),
		RequestID:   requestctx.RequestID(c.Context()),
		Source:      audit.SourceHTTP,
		Outcome:     audit.OutcomeSucceeded,
		StatusCode:  status,
		CompletedAt: now,
		CreatedAt:   now,
	}
	if actorID == uuid.Nil {
		event.ActorType = audit.ActorTypeAnonymous
	} else if event.ActorType == audit.ActorTypeUnknown {
		event.ActorType = audit.ActorTypeUser
	}
	return event
}

// recordFallback writes the event on a detached, bounded context. The
// response is already final, so a failure is counted and logged only.
func recordFallback(c fiber.Ctx, recorder audit.Recorder, event audit.Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), audit.WriteTimeout)
	defer cancel()
	if err := recorder.Record(ctx, event); err != nil {
		metrics.AuditWriteFailuresTotal.Inc()
		slog.WarnContext(c.Context(), "failed to record audit event",
			slog.String("action", event.Action),
			slog.Any("error", err),
		)
	}
}

// routeResourceID is the mutated resource's id. Feature-flag routes name the
// target :key rather than :id.
func routeResourceID(c fiber.Ctx) string {
	if id := c.Params("id"); id != "" {
		return id
	}
	return c.Params("key")
}

// requestRole returns the authenticated subject's role, or "" for an
// anonymous/system request. It reads the authz context instead of a Fiber
// local mirror.
func requestRole(c fiber.Ctx) string {
	subject, ok := authz.SubjectFromContext(c.Context())
	if !ok {
		return ""
	}
	return subject.Role
}
