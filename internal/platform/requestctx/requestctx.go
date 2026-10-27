// Package requestctx carries request correlation data through a context so
// services, jobs, and events can populate metadata without importing Fiber.
package requestctx

import (
	"context"

	"github.com/google/uuid"
)

type contextKey struct{}

type values struct {
	requestID string
	path      string
	actorID   uuid.UUID
	sessionID uuid.UUID
	client    ClientInfo
}

// ClientInfo is the transport metadata every request carries: the client IP
// (resolved through Fiber's trusted-proxy configuration) and the User-Agent.
// Handlers pass these to services for login throttling, session records, and
// audit metadata; they are never read from HTTP state below the handler layer.
type ClientInfo struct {
	IP        string
	UserAgent string
}

// WithClientInfo stores the request's client metadata in ctx. The platform
// middleware sets it once per request.
func WithClientInfo(ctx context.Context, info ClientInfo) context.Context {
	v, _ := ctx.Value(contextKey{}).(values)
	v.client = info
	return context.WithValue(ctx, contextKey{}, v)
}

// ClientInfoFromContext returns the stored client metadata, or the zero value
// for internal callers and tests.
func ClientInfoFromContext(ctx context.Context) ClientInfo {
	v, _ := ctx.Value(contextKey{}).(values)
	return v.client
}

// WithRequestID stores the request correlation id in ctx.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	v, _ := ctx.Value(contextKey{}).(values)
	v.requestID = requestID
	return context.WithValue(ctx, contextKey{}, v)
}

// RequestID returns the stored request id, or "".
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(contextKey{}).(values)
	return v.requestID
}

// WithPath stores the request path for problem-document instances. It is set
// once by the request-ID middleware.
func WithPath(ctx context.Context, path string) context.Context {
	v, _ := ctx.Value(contextKey{}).(values)
	v.path = path
	return context.WithValue(ctx, contextKey{}, v)
}

// Path returns the stored request path, or "".
func Path(ctx context.Context) string {
	v, _ := ctx.Value(contextKey{}).(values)
	return v.path
}

// WithActorID stores the authenticated actor id in ctx. A zero id clears the
// actor, which keeps system and anonymous work explicit.
func WithActorID(ctx context.Context, actorID uuid.UUID) context.Context {
	v, _ := ctx.Value(contextKey{}).(values)
	v.actorID = actorID
	return context.WithValue(ctx, contextKey{}, v)
}

// ActorID returns the stored actor id, or 0.
func ActorID(ctx context.Context) uuid.UUID {
	v, _ := ctx.Value(contextKey{}).(values)
	return v.actorID
}

// WithSessionID stores the authenticated session id in ctx. Framework-neutral
// handlers (huma) use it for operations that mutate the caller's session, such
// as an MFA step-up.
func WithSessionID(ctx context.Context, sessionID uuid.UUID) context.Context {
	v, _ := ctx.Value(contextKey{}).(values)
	v.sessionID = sessionID
	return context.WithValue(ctx, contextKey{}, v)
}

// SessionID returns the stored session id, or 0 when the caller is not a JWT
// session (for example an API key or a public route).
func SessionID(ctx context.Context) uuid.UUID {
	v, _ := ctx.Value(contextKey{}).(values)
	return v.sessionID
}
