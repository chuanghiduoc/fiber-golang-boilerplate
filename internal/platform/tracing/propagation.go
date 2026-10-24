package tracing

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// TraceParentHeader is the W3C Trace Context header persisted with a job so a
// worker can link its processing span back to the request that enqueued it.
const TraceParentHeader = "traceparent"

// CaptureRemoteContext returns the W3C traceparent header for the active span
// in ctx, or "" when the context has no valid span. It never returns any other
// context value, so jobs cannot accidentally persist headers or payload data.
func CaptureRemoteContext(ctx context.Context) string {
	if ctx == nil || !trace.SpanContextFromContext(ctx).IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get(TraceParentHeader)
}

// ExtractRemoteContext parses a W3C traceparent value and returns a context
// carrying it as a remote parent. Malformed or empty values are ignored, so a
// corrupt jobs.trace_parent column cannot break job processing.
func ExtractRemoteContext(ctx context.Context, traceparent string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if traceparent == "" {
		return ctx
	}
	carrier := propagation.MapCarrier{TraceParentHeader: traceparent}
	return propagation.TraceContext{}.Extract(ctx, carrier)
}
