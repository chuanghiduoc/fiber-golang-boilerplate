package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

func TestHandlerEnrichesFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(Wrap(slog.NewJSONHandler(&buf, nil)))

	tenantID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000001")
	userID := uuid.MustParse("0192c9b0-7e4a-7000-8000-000000000002")
	traceID := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	spanID := trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}
	spanCtx := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})

	ctx := requestctx.WithRequestID(context.Background(), "req-1")
	ctx = requestctx.WithActorID(ctx, userID)
	ctx = tenant.WithID(ctx, tenantID)
	ctx = trace.ContextWithSpanContext(ctx, spanCtx)

	logger.InfoContext(ctx, "hello", slog.String("user_id", "caller-wins"))

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v (raw=%s)", err, buf.String())
	}
	if record["request_id"] != "req-1" {
		t.Errorf("request_id = %v, want req-1", record["request_id"])
	}
	if record["tenant_id"] != tenantID.String() {
		t.Errorf("tenant_id = %v, want %s", record["tenant_id"], tenantID)
	}
	if record["trace_id"] != traceID.String() {
		t.Errorf("trace_id = %v, want %s", record["trace_id"], traceID)
	}
	if record["span_id"] != spanID.String() {
		t.Errorf("span_id = %v, want %s", record["span_id"], spanID)
	}
	if record["user_id"] != "caller-wins" {
		t.Errorf("user_id = %v, want the call-site value", record["user_id"])
	}
}

func TestHandlerWithoutContextAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(Wrap(slog.NewJSONHandler(&buf, nil)))
	logger.Info("plain")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v", err)
	}
	for _, key := range []string{"request_id", "tenant_id", "user_id", "trace_id"} {
		if _, ok := record[key]; ok {
			t.Errorf("plain record must not carry %s", key)
		}
	}
}
