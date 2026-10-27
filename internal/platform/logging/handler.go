// Package logging installs the process logger with a handler that enriches
// every record from the caller's context. Request correlation, the active
// trace span, the tenant, and the authenticated actor are therefore present on
// every log line without each call site repeating them.
package logging

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// Wrap decorates base so records logged with a context carry the request's
// correlation data. An attribute the record already sets wins, so explicit
// values at a call site are never duplicated or overridden.
func Wrap(base slog.Handler) slog.Handler {
	return &contextHandler{base: base}
}

type contextHandler struct {
	base slog.Handler
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	attrs := make([]slog.Attr, 0, 5)
	if requestID := requestctx.RequestID(ctx); requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}
	if spanCtx := trace.SpanContextFromContext(ctx); spanCtx.IsValid() {
		attrs = append(attrs, slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()))
	}
	if tenantID := tenant.IDFromContext(ctx); tenantID != uuid.Nil {
		attrs = append(attrs, slog.String("tenant_id", tenantID.String()))
	}
	if actorID := requestctx.ActorID(ctx); actorID != uuid.Nil {
		attrs = append(attrs, slog.String("user_id", actorID.String()))
	}
	if len(attrs) == 0 {
		return h.base.Handle(ctx, record)
	}

	present := make(map[string]struct{}, 8)
	record.Attrs(func(attr slog.Attr) bool {
		present[attr.Key] = struct{}{}
		return true
	})
	for _, attr := range attrs {
		if _, ok := present[attr.Key]; ok {
			continue
		}
		record.AddAttrs(attr)
	}
	return h.base.Handle(ctx, record)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{base: h.base.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{base: h.base.WithGroup(name)}
}
