package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

type requestInfoKey struct{}

// RequestInfo carries request-scoped audit identity from middleware into
// services. Guard stores a pointer so a service event can mark that a domain
// event was recorded without changing the request context.
type RequestInfo struct {
	EventID            int64
	TenantID           int64
	ActorID            int64
	ActorType          string
	ActorRole          string
	RequestID          string
	IP                 string
	UserAgent          string
	Action             string
	Resource           string
	IdempotencyKeyHash string
	// DomainEvents counts service-level events created from this request. A
	// Guard with RequireDomain uses it to detect a route that mutated without
	// writing its domain audit row.
	DomainEvents int
	// ServiceRecorded counts Tier 1 service events successfully written by
	// RecordInTx for this request. The fallback Middleware skips a request
	// with ServiceRecorded > 0 so one mutation never yields two audit rows.
	ServiceRecorded int
}

// WithRequestInfo stores info in ctx.
func WithRequestInfo(ctx context.Context, info *RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, info)
}

// RequestInfoFromContext returns the request audit info, or nil.
func RequestInfoFromContext(ctx context.Context) *RequestInfo {
	if info, ok := ctx.Value(requestInfoKey{}).(*RequestInfo); ok {
		return info
	}
	return nil
}

// WithActor enriches the request audit identity with the authenticated actor.
// It is safe to call before Guard: Guard preserves these values in the intent
// and completion rows.
func WithActor(ctx context.Context, actorID int64, role string) context.Context {
	info := RequestInfoFromContext(ctx)
	if info == nil {
		info = &RequestInfo{}
		ctx = WithRequestInfo(ctx, info)
	}
	info.ActorID = actorID
	info.ActorRole = role
	info.ActorType = ActorTypeForRole(role)
	if actorID != 0 && info.ActorType == ActorTypeUnknown {
		info.ActorType = ActorTypeUser
	}
	return ctx
}

// WithRequestMeta enriches the request audit identity with transport metadata.
// Values are cloned because callers pass Fiber strings (c.IP(), c.Get), which
// are zero-copy views into pooled request buffers, while RequestInfo outlives
// the handler in the context.
func WithRequestMeta(ctx context.Context, requestID, ip, userAgent string) context.Context {
	info := RequestInfoFromContext(ctx)
	if info == nil {
		info = &RequestInfo{}
		ctx = WithRequestInfo(ctx, info)
	}
	if requestID != "" {
		info.RequestID = strings.Clone(requestID)
	}
	if ip != "" {
		info.IP = strings.Clone(ip)
	}
	if userAgent != "" {
		info.UserAgent = strings.Clone(userAgent)
	}
	return ctx
}

// EventIDFromContext returns the pre-write intent row ID associated with ctx.
func EventIDFromContext(ctx context.Context) int64 {
	if info := RequestInfoFromContext(ctx); info != nil {
		return info.EventID
	}
	return 0
}

// NewServiceEvent builds a terminal service event enriched from the request
// context. Services should set ResourceID and any non-sensitive metadata before
// calling RecordTx. Creating the event marks the request as having a domain
// audit record for Guard's RequireDomain check.
func NewServiceEvent(ctx context.Context, action, resource string) Event {
	now := time.Now().UTC()
	event := Event{
		Action:    action,
		Resource:  resource,
		Source:    SourceService,
		ActorType: ActorTypeService,
		Outcome:   OutcomeSucceeded,
		CreatedAt: now,
	}
	info := RequestInfoFromContext(ctx)
	if info == nil {
		event.TenantID = tenant.EffectiveID(ctx)
		return event
	}
	info.DomainEvents++
	event.TenantID = info.TenantID
	if event.TenantID == 0 {
		event.TenantID = tenant.EffectiveID(ctx)
	}
	event.ParentID = info.EventID
	event.ActorID = info.ActorID
	event.ActorRole = info.ActorRole
	if info.ActorType != "" && info.ActorType != ActorTypeUnknown {
		event.ActorType = info.ActorType
	} else if event.ActorID != 0 {
		event.ActorType = ActorTypeUser
	}
	event.RequestID = info.RequestID
	event.IP = info.IP
	event.UserAgent = info.UserAgent
	event.IdempotencyKeyHash = info.IdempotencyKeyHash
	return event
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
