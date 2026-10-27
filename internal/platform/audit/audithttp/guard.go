// Package audithttp is the Fiber adapter for internal/platform/audit: the
// route-level Guard (Tier 2 intent rows) and the fallback Middleware. Services
// record through package audit and never import this package.
package audithttp

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// Mode controls what happens when the pre-write intent cannot be stored.
type Mode string

const (
	// idempotencyReplayedLocal is set by the idempotency middleware when a
	// cached response is replayed. The middleware cannot import this package,
	// so the key is duplicated here.
	idempotencyReplayedLocal = "idempotency_replayed"
	// auditGuardedLocal lets the global audit fallback middleware skip routes
	// that already wrote and completed an intent row.
	auditGuardedLocal = "audit_guarded"

	// ModeRequired fails the request with system.audit_unavailable when the
	// intent cannot be stored. It is the default: privileged mutations are
	// fail-closed.
	ModeRequired Mode = "required"
	// ModeBestEffort logs intent failures and still runs the handler. It must
	// be chosen explicitly.
	ModeBestEffort Mode = "best_effort"
)

// Policy configures a route-level audit guard.
type Policy struct {
	// Mode defaults to ModeRequired when empty; any value other than
	// ModeBestEffort is treated as ModeRequired so a typo cannot fail open.
	Mode Mode
	// RequireDomain requires the handler to create an explicit service event.
	// A successful mutation without one completes the intent as unknown and
	// increments audit_domain_missing_total.
	RequireDomain bool
	// Action defaults to "<METHOD> <route>".
	Action string
	// Resource defaults to the route template.
	Resource string
}

func (p Policy) effectiveMode() Mode {
	if p.Mode == ModeBestEffort {
		return ModeBestEffort
	}
	return ModeRequired
}

// guardTarget names what a guarded request mutates.
type guardTarget struct {
	routePath string
	action    string
	resource  string
}

func (p Policy) target(c fiber.Ctx) guardTarget {
	t := guardTarget{routePath: c.Route().Path, action: p.Action, resource: p.Resource}
	if t.routePath == "" {
		t.routePath = c.Path()
	}
	if t.action == "" {
		t.action = c.Method() + " " + t.routePath
	}
	if t.resource == "" {
		t.resource = t.routePath
	}
	return t
}

// Guard writes a pre-write intent row for a mutating route, then completes it
// with the observed outcome after the handler runs. It must run after
// Idempotency so replayed requests are not audited again.
//
// A required Guard needs a recorder that implements [IDRecorder] and
// [CompletionRecorder]. Noop deliberately implements neither, so a
// misconfigured required route returns 503 instead of accepting an unaudited
// mutation.
func Guard(recorder audit.Recorder, policy Policy, catalogs ...*apperror.Catalog) fiber.Handler {
	catalog := apperror.PlatformCatalog()
	if len(catalogs) > 0 && catalogs[0] != nil {
		catalog = catalogs[0]
	}
	if recorder == nil {
		recorder = audit.Noop{}
	}
	mode := policy.effectiveMode()

	return func(c fiber.Ctx) error {
		// Idempotency middleware normally short-circuits a replay before this
		// handler runs. Check the local as well so an unusual middleware order
		// cannot create a second audit row for a replayed mutation.
		if fiber.Locals[bool](c, idempotencyReplayedLocal) {
			return c.Next()
		}

		target := policy.target(c)
		eventID, err := beginIntent(c, recorder, mode, target)
		if err != nil {
			return err
		}

		info := requestInfoFor(c, eventID, target.action, target.resource)
		c.SetContext(audit.WithRequestInfo(c.Context(), info))
		fiber.Locals[uuid.UUID](c, "audit_event_id", eventID)
		fiber.Locals[bool](c, auditGuardedLocal, true)

		if eventID == uuid.Nil {
			// Best-effort mode with no intent row: nothing to complete.
			return c.Next()
		}
		return runAndCompleteIntent(c, recorder, policy, eventID, info, catalog)
	}
}

// beginIntent stores the started intent row and returns its id. When the row
// cannot be stored it returns system.audit_unavailable in required mode and a
// zero id with no error in best-effort mode.
func beginIntent(c fiber.Ctx, recorder audit.Recorder, mode Mode, target guardTarget) (uuid.UUID, error) {
	idRecorder, ok := recorder.(audit.IDRecorder)
	if !ok {
		metrics.AuditIntentFailuresTotal.Inc()
		slog.ErrorContext(c.Context(), "audit guard recorder cannot write intent rows",
			slog.String("action", target.action),
			slog.String("mode", string(mode)),
			slog.String("request_id", requestctx.RequestID(c.Context())),
		)
		return uuid.Nil, intentUnavailable(mode)
	}

	intent := intentEvent(c, target)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), audit.WriteTimeout)
	defer cancel()
	id, err := idRecorder.RecordID(ctx, intent)
	if err != nil {
		metrics.AuditIntentFailuresTotal.Inc()
		slog.WarnContext(c.Context(), "failed to write audit intent row",
			slog.String("action", target.action),
			slog.String("request_id", intent.RequestID),
			slog.Any("error", err),
		)
		return uuid.Nil, intentUnavailable(mode)
	}
	return id, nil
}

func intentUnavailable(mode Mode) error {
	if mode == ModeRequired {
		return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
	}
	return nil
}

func intentEvent(c fiber.Ctx, target guardTarget) audit.Event {
	info := requestInfoFor(c, uuid.Nil, target.action, target.resource)
	intent := audit.Event{
		TenantID:  info.TenantID,
		ActorID:   info.ActorID,
		ActorType: info.ActorType,
		ActorRole: info.ActorRole,
		Action:    target.action,
		Resource:  target.resource,
		// Like Middleware, fall back to :key so a denied feature-flag
		// attempt still records its target.
		ResourceID:         routeResourceID(c),
		Metadata:           map[string]any{"method": c.Method(), "route": target.routePath},
		IP:                 info.IP,
		UserAgent:          info.UserAgent,
		RequestID:          info.RequestID,
		Source:             audit.SourceHTTP,
		Outcome:            audit.OutcomeStarted,
		IdempotencyKeyHash: info.IdempotencyKeyHash,
	}
	if intent.ActorType == "" {
		intent.ActorType = audit.ActorTypeAnonymous
	}
	return intent
}

// runAndCompleteIntent runs the handler and completes the intent row with the
// observed outcome, including a panic, which is recorded and re-raised.
func runAndCompleteIntent(c fiber.Ctx, recorder audit.Recorder, policy Policy, eventID uuid.UUID, info *audit.RequestInfo, catalog *apperror.Catalog) error {
	defer func() {
		if recovered := recover(); recovered != nil {
			completeGuarded(c, recorder, eventID, audit.Completion{
				Outcome:    audit.OutcomeFailed,
				StatusCode: fiber.StatusInternalServerError,
				ErrorCode:  "panic",
			})
			panic(recovered)
		}
	}()

	handlerErr := c.Next()
	completion := classifyOutcome(c, handlerErr, info, catalog)
	if policy.RequireDomain && completion.Outcome == audit.OutcomeSucceeded && info.DomainEvents == 0 {
		metrics.AuditDomainMissingTotal.Inc()
		completion.Outcome = audit.OutcomeUnknown
		completion.ErrorCode = "audit.domain_missing"
		slog.WarnContext(c.Context(), "guarded mutation completed without a domain audit event",
			slog.String("action", info.Action),
			slog.String("request_id", info.RequestID),
		)
	}
	completeGuarded(c, recorder, eventID, completion)
	return handlerErr
}

func completeGuarded(c fiber.Ctx, recorder audit.Recorder, eventID uuid.UUID, completion audit.Completion) {
	completer, ok := recorder.(audit.CompletionRecorder)
	if !ok {
		metrics.AuditCompletionFailuresTotal.Inc()
		slog.ErrorContext(c.Context(), "audit guard recorder cannot complete intent rows",
			slog.String("audit_event_id", eventID.String()),
			slog.String("request_id", requestctx.RequestID(c.Context())),
		)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), audit.WriteTimeout)
	defer cancel()
	if err := completer.Complete(ctx, eventID, completion); err != nil {
		metrics.AuditCompletionFailuresTotal.Inc()
		slog.WarnContext(c.Context(), "failed to complete audit intent row",
			slog.String("audit_event_id", eventID.String()),
			slog.String("request_id", requestctx.RequestID(c.Context())),
			slog.Any("error", err),
		)
	}
}

func classifyOutcome(c fiber.Ctx, handlerErr error, info *audit.RequestInfo, catalog *apperror.Catalog) audit.Completion {
	completion := audit.Completion{Outcome: audit.OutcomeSucceeded, StatusCode: c.Response().StatusCode()}
	if completion.StatusCode == 0 {
		completion.StatusCode = fiber.StatusOK
	}
	if handlerErr == nil {
		switch {
		case completion.StatusCode >= fiber.StatusOK && completion.StatusCode < fiber.StatusMultipleChoices:
			completion.Outcome = audit.OutcomeSucceeded
		case completion.StatusCode == fiber.StatusUnauthorized || completion.StatusCode == fiber.StatusForbidden:
			completion.Outcome = audit.OutcomeDenied
			completion.ErrorCode = statusErrorCode(completion.StatusCode)
		default:
			completion.Outcome = audit.OutcomeFailed
			completion.ErrorCode = statusErrorCode(completion.StatusCode)
		}
		return completion
	}

	var appErr *apperror.AppError
	if errors.As(handlerErr, &appErr) {
		_, spec := catalog.Resolve(apperror.Code(appErr.Code))
		completion.StatusCode = spec.Status
		completion.ErrorCode = appErr.Code
	} else {
		var fiberErr *fiber.Error
		if errors.As(handlerErr, &fiberErr) {
			completion.StatusCode = fiberErr.Code
		} else {
			completion.StatusCode = fiber.StatusInternalServerError
		}
		completion.ErrorCode = statusErrorCode(completion.StatusCode)
	}
	if completion.StatusCode == fiber.StatusUnauthorized || completion.StatusCode == fiber.StatusForbidden {
		completion.Outcome = audit.OutcomeDenied
	} else {
		completion.Outcome = audit.OutcomeFailed
	}
	return completion
}

func statusErrorCode(status int) string {
	if status >= 200 && status < 300 {
		return ""
	}
	return apperror.NewForStatus(status, "").Code
}

func requestInfoFor(c fiber.Ctx, eventID uuid.UUID, action, resource string) *audit.RequestInfo {
	// IP and User-Agent are cloned: Fiber strings are zero-copy views into
	// pooled buffers, while RequestInfo lives in the context beyond the handler.
	info := &audit.RequestInfo{
		EventID:            eventID,
		TenantID:           tenanthttp.ID(c),
		Action:             action,
		Resource:           resource,
		RequestID:          requestctx.RequestID(c.Context()),
		IP:                 strings.Clone(c.IP()),
		UserAgent:          strings.Clone(c.Get("User-Agent")),
		IdempotencyKeyHash: audit.HashIdempotencyKey(c.Get("Idempotency-Key")),
	}
	if info.TenantID == uuid.Nil {
		info.TenantID = tenant.IDFromContext(c.Context())
	}
	if existing := audit.RequestInfoFromContext(c.Context()); existing != nil {
		if existing.ActorID != uuid.Nil {
			info.ActorID = existing.ActorID
		}
		if existing.ActorType != "" && existing.ActorType != audit.ActorTypeUnknown {
			info.ActorType = existing.ActorType
		}
		if existing.ActorRole != "" {
			info.ActorRole = existing.ActorRole
		}
	}
	if info.ActorID == uuid.Nil {
		info.ActorID = requestctx.ActorID(c.Context())
	}
	if info.ActorRole == "" {
		info.ActorRole = requestRole(c)
	}
	if info.ActorType == "" || info.ActorType == audit.ActorTypeUnknown {
		if info.ActorID != uuid.Nil {
			info.ActorType = audit.ActorTypeForRole(info.ActorRole)
			if info.ActorType == audit.ActorTypeUnknown {
				info.ActorType = audit.ActorTypeUser
			}
		} else {
			info.ActorType = audit.ActorTypeAnonymous
		}
	}
	return info
}
