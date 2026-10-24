// Package requestctx carries request correlation data through a context so
// services, jobs, and events can populate metadata without importing Fiber.
package requestctx

import "context"

type contextKey struct{}

type values struct {
	requestID string
	actorID   int64
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

// WithActorID stores the authenticated actor id in ctx. A zero id clears the
// actor, which keeps system and anonymous work explicit.
func WithActorID(ctx context.Context, actorID int64) context.Context {
	v, _ := ctx.Value(contextKey{}).(values)
	v.actorID = actorID
	return context.WithValue(ctx, contextKey{}, v)
}

// ActorID returns the stored actor id, or 0.
func ActorID(ctx context.Context) int64 {
	v, _ := ctx.Value(contextKey{}).(values)
	return v.actorID
}
