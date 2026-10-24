package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
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
func Guard(recorder Recorder, policy Policy) fiber.Handler {
	if recorder == nil {
		recorder = Noop{}
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
		c.SetContext(WithRequestInfo(c.Context(), info))
		fiber.Locals[int64](c, "audit_event_id", eventID)
		fiber.Locals[bool](c, auditGuardedLocal, true)

		if eventID == 0 {
			// Best-effort mode with no intent row: nothing to complete.
			return c.Next()
		}
		return runAndCompleteIntent(c, recorder, policy, eventID, info)
	}
}

// beginIntent stores the started intent row and returns its id. When the row
// cannot be stored it returns system.audit_unavailable in required mode and a
// zero id with no error in best-effort mode.
func beginIntent(c fiber.Ctx, recorder Recorder, mode Mode, target guardTarget) (int64, error) {
	idRecorder, ok := recorder.(IDRecorder)
	if !ok {
		metrics.AuditIntentFailuresTotal.Inc()
		slog.Error("audit guard recorder cannot write intent rows",
			slog.String("action", target.action),
			slog.String("mode", string(mode)),
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
		)
		return 0, intentUnavailable(mode)
	}

	intent := intentEvent(c, target)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), WriteTimeout)
	defer cancel()
	id, err := idRecorder.RecordID(ctx, intent)
	if err != nil {
		metrics.AuditIntentFailuresTotal.Inc()
		slog.Warn("failed to write audit intent row",
			slog.String("action", target.action),
			slog.String("request_id", intent.RequestID),
			slog.Any("error", err),
		)
		return 0, intentUnavailable(mode)
	}
	return id, nil
}

func intentUnavailable(mode Mode) error {
	if mode == ModeRequired {
		return apperror.NewCode(apperror.CodeSystemAuditUnavailable, "audit service unavailable")
	}
	return nil
}

func intentEvent(c fiber.Ctx, target guardTarget) Event {
	info := requestInfoFor(c, 0, target.action, target.resource)
	intent := Event{
		TenantID:  info.TenantID,
		ActorID:   info.ActorID,
		ActorType: info.ActorType,
		ActorRole: info.ActorRole,
		Action:    target.action,
		Resource:  target.resource,
		// Like audit.Middleware, fall back to :key so a denied feature-flag
		// attempt still records its target.
		ResourceID:         routeResourceID(c),
		Metadata:           map[string]any{"method": c.Method(), "route": target.routePath},
		IP:                 info.IP,
		UserAgent:          info.UserAgent,
		RequestID:          info.RequestID,
		Source:             SourceHTTP,
		Outcome:            OutcomeStarted,
		IdempotencyKeyHash: info.IdempotencyKeyHash,
	}
	if intent.ActorType == "" {
		intent.ActorType = ActorTypeAnonymous
	}
	return intent
}

// runAndCompleteIntent runs the handler and completes the intent row with the
// observed outcome, including a panic, which is recorded and re-raised.
func runAndCompleteIntent(c fiber.Ctx, recorder Recorder, policy Policy, eventID int64, info *RequestInfo) error {
	defer func() {
		if recovered := recover(); recovered != nil {
			completeGuarded(c, recorder, eventID, Completion{
				Outcome:    OutcomeFailed,
				StatusCode: fiber.StatusInternalServerError,
				ErrorCode:  "panic",
			})
			panic(recovered)
		}
	}()

	handlerErr := c.Next()
	completion := classifyOutcome(c, handlerErr, info)
	if policy.RequireDomain && completion.Outcome == OutcomeSucceeded && info.DomainEvents == 0 {
		metrics.AuditDomainMissingTotal.Inc()
		completion.Outcome = OutcomeUnknown
		completion.ErrorCode = "audit.domain_missing"
		slog.Warn("guarded mutation completed without a domain audit event",
			slog.String("action", info.Action),
			slog.String("request_id", info.RequestID),
		)
	}
	completeGuarded(c, recorder, eventID, completion)
	return handlerErr
}

func completeGuarded(c fiber.Ctx, recorder Recorder, eventID int64, completion Completion) {
	completer, ok := recorder.(CompletionRecorder)
	if !ok {
		metrics.AuditCompletionFailuresTotal.Inc()
		slog.Error("audit guard recorder cannot complete intent rows",
			slog.Int64("audit_event_id", eventID),
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
		)
		return
	}
	completion.ActorID = fiber.Locals[int64](c, "user_id")
	role := fiber.Locals[string](c, "role")
	completion.ActorRole = role
	completion.ActorType = ActorTypeForRole(role)
	if completion.ActorID == 0 {
		completion.ActorType = ActorTypeAnonymous
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), WriteTimeout)
	defer cancel()
	if err := completer.Complete(ctx, eventID, completion); err != nil {
		metrics.AuditCompletionFailuresTotal.Inc()
		slog.Warn("failed to complete audit intent row",
			slog.Int64("audit_event_id", eventID),
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Any("error", err),
		)
	}
}

func classifyOutcome(c fiber.Ctx, handlerErr error, info *RequestInfo) Completion {
	completion := Completion{Outcome: OutcomeSucceeded, StatusCode: c.Response().StatusCode()}
	if completion.StatusCode == 0 {
		completion.StatusCode = fiber.StatusOK
	}
	if handlerErr == nil {
		switch {
		case completion.StatusCode >= fiber.StatusOK && completion.StatusCode < fiber.StatusMultipleChoices:
			completion.Outcome = OutcomeSucceeded
		case completion.StatusCode == fiber.StatusUnauthorized || completion.StatusCode == fiber.StatusForbidden:
			completion.Outcome = OutcomeDenied
			completion.ErrorCode = statusErrorCode(completion.StatusCode)
		default:
			completion.Outcome = OutcomeFailed
			completion.ErrorCode = statusErrorCode(completion.StatusCode)
		}
		return completion
	}

	var appErr *apperror.AppError
	if errors.As(handlerErr, &appErr) {
		completion.StatusCode = appErr.Status
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
		completion.Outcome = OutcomeDenied
	} else {
		completion.Outcome = OutcomeFailed
	}
	return completion
}

func statusErrorCode(status int) string {
	if status >= 200 && status < 300 {
		return ""
	}
	title := http.StatusText(status)
	if title == "" {
		return "internal_error"
	}
	return strings.ToLower(strings.ReplaceAll(title, " ", "_"))
}

func requestInfoFor(c fiber.Ctx, eventID int64, action, resource string) *RequestInfo {
	// IP and User-Agent are cloned: Fiber strings are zero-copy views into
	// pooled buffers, while RequestInfo lives in the context beyond the handler.
	info := &RequestInfo{
		EventID:            eventID,
		TenantID:           tenant.ID(c),
		Action:             action,
		Resource:           resource,
		RequestID:          fiber.Locals[string](c, "request_id"),
		IP:                 strings.Clone(c.IP()),
		UserAgent:          strings.Clone(c.Get("User-Agent")),
		IdempotencyKeyHash: HashIdempotencyKey(c.Get("Idempotency-Key")),
	}
	if info.TenantID == 0 {
		info.TenantID = tenant.EffectiveID(c.Context())
	}
	if existing := RequestInfoFromContext(c.Context()); existing != nil {
		if existing.ActorID != 0 {
			info.ActorID = existing.ActorID
		}
		if existing.ActorType != "" && existing.ActorType != ActorTypeUnknown {
			info.ActorType = existing.ActorType
		}
		if existing.ActorRole != "" {
			info.ActorRole = existing.ActorRole
		}
	}
	if info.ActorID == 0 {
		info.ActorID = fiber.Locals[int64](c, "user_id")
	}
	if info.ActorRole == "" {
		info.ActorRole = fiber.Locals[string](c, "role")
	}
	if info.ActorType == "" || info.ActorType == ActorTypeUnknown {
		if info.ActorID != 0 {
			info.ActorType = ActorTypeForRole(info.ActorRole)
			if info.ActorType == ActorTypeUnknown {
				info.ActorType = ActorTypeUser
			}
		} else {
			info.ActorType = ActorTypeAnonymous
		}
	}
	return info
}

// HashIdempotencyKey returns a stable SHA-256 digest for an Idempotency-Key.
// The raw key is client-controlled and must never be stored in the trail.
func HashIdempotencyKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
