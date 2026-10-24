package middleware

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func newTracingTestApp(t *testing.T) (*fiber.App, *tracetest.SpanRecorder) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(RequestID())
	app.Use(Tracing(provider, []string{"/healthz", "/readyz", "/health", "/metrics"}, true))
	return app, recorder
}

func TestTracingMiddlewareRecordsRouteTemplate(t *testing.T) {
	app, recorder := newTracingTestApp(t)
	app.Get("/users/:id", func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "user_id", 7)
		fiber.Locals[int64](c, "tenant_id", 2)
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/users/42?email=user@example.com&token=super-secret", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	span := ended[0]

	assert.Equal(t, "GET /users/:id", span.Name())
	assert.Equal(t, trace.SpanKindServer, span.SpanKind())
	assert.Equal(t, "GET", attrString(span.Attributes(), semconv.HTTPRequestMethodKey))
	assert.Equal(t, "/users/:id", attrString(span.Attributes(), semconv.HTTPRouteKey))
	assert.Equal(t, int64(200), attrInt(span.Attributes(), semconv.HTTPResponseStatusCodeKey))
	assert.NotEmpty(t, attrString(span.Attributes(), attribute.Key("app.request_id")))
	assert.Equal(t, "7", attrString(span.Attributes(), semconv.EnduserIDKey))
	assert.Equal(t, int64(2), attrInt(span.Attributes(), attribute.Key("tenant.id")))

	// Raw path segments, query parameters, and secrets must never be attached.
	for _, attr := range span.Attributes() {
		value := attr.Value.String()
		assert.NotContains(t, value, "/users/42")
		assert.NotContains(t, value, "user@example.com")
		assert.NotContains(t, value, "super-secret")
	}
}

func TestTracingMiddlewareExtractsW3CTraceParent(t *testing.T) {
	app, recorder := newTracingTestApp(t)
	app.Get("/traced", func(c fiber.Ctx) error { return c.SendString("ok") })

	const (
		traceID      = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentSpanID = "00f067aa0ba902b7"
		traceparent  = "00-" + traceID + "-" + parentSpanID + "-01"
	)
	req, _ := http.NewRequest("GET", "/traced", http.NoBody)
	req.Header.Set("traceparent", traceparent)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	span := ended[0]
	assert.Equal(t, traceID, span.SpanContext().TraceID().String())
	assert.Equal(t, parentSpanID, span.Parent().SpanID().String())
	assert.True(t, span.Parent().IsRemote())
	assert.True(t, span.SpanContext().IsSampled())
}

func TestTracingMiddlewareIgnoresUntrustedTraceParent(t *testing.T) {
	const (
		traceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
		traceparent = "00-" + traceID + "-00f067aa0ba902b7-01"
	)
	cases := map[string]struct {
		cfg           fiber.Config
		trustIncoming bool
	}{
		"flag disabled": {cfg: fiber.Config{}, trustIncoming: false},
		// app.Test connects from 0.0.0.0, which is not in the allowlist.
		"peer is not a trusted proxy": {cfg: fiber.Config{
			TrustProxy:       true,
			TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"10.0.0.1"}},
		}, trustIncoming: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(
				sdktrace.WithSpanProcessor(recorder),
				sdktrace.WithSampler(sdktrace.AlwaysSample()),
			)
			t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

			tc.cfg.ErrorHandler = apperror.FiberErrorHandler
			app := fiber.New(tc.cfg)
			app.Use(Tracing(provider, nil, tc.trustIncoming))
			app.Get("/traced", func(c fiber.Ctx) error { return c.SendString("ok") })

			req, _ := http.NewRequest("GET", "/traced", http.NoBody)
			req.Header.Set("traceparent", traceparent)
			resp, err := app.Test(req)
			require.NoError(t, err)
			_ = resp.Body.Close()

			ended := recorder.Ended()
			require.Len(t, ended, 1)
			assert.NotEqual(t, traceID, ended[0].SpanContext().TraceID().String())
			assert.False(t, ended[0].Parent().IsValid(), "untrusted traceparent must not become the parent")
		})
	}
}

func TestTracingMiddlewareSkipsExcludedPaths(t *testing.T) {
	app, recorder := newTracingTestApp(t)
	app.Get("/healthz", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/metrics", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/api", func(c fiber.Ctx) error { return c.SendString("ok") })

	for _, path := range []string{"/healthz", "/metrics"} {
		req, _ := http.NewRequest("GET", path, http.NoBody)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		_ = resp.Body.Close()
	}
	assert.Empty(t, recorder.Ended(), "probe endpoints must not create spans")

	req, _ := http.NewRequest("GET", "/api", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
	assert.Len(t, recorder.Ended(), 1)
}

func TestTracingMiddlewareErrorSemantics(t *testing.T) {
	t.Run("5xx marks error without raw message", func(t *testing.T) {
		app, recorder := newTracingTestApp(t)
		app.Get("/fail", func(fiber.Ctx) error {
			return apperror.NewInternal("smtp rejected user@example.com with token=super-secret")
		})

		req, _ := http.NewRequest("GET", "/fail", http.NoBody)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		_ = resp.Body.Close()

		ended := recorder.Ended()
		require.Len(t, ended, 1)
		span := ended[0]
		assert.Equal(t, int64(500), attrInt(span.Attributes(), semconv.HTTPResponseStatusCodeKey))
		assert.Equal(t, codes.Error, span.Status().Code)
		assert.Empty(t, span.Status().Description)
		assert.Empty(t, span.Events())
		assert.Equal(t, "AppError", attrString(span.Attributes(), semconv.ErrorTypeKey))
		assert.Equal(t, "internal_error", attrString(span.Attributes(), attribute.Key("app.error_code")))
		for _, attr := range span.Attributes() {
			assert.NotContains(t, attr.Value.String(), "user@example.com")
			assert.NotContains(t, attr.Value.String(), "super-secret")
		}
	})

	t.Run("4xx leaves status unset", func(t *testing.T) {
		app, recorder := newTracingTestApp(t)
		app.Get("/missing", func(fiber.Ctx) error {
			return apperror.NewNotFound("user user@example.com not found")
		})

		req, _ := http.NewRequest("GET", "/missing", http.NoBody)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		_ = resp.Body.Close()

		ended := recorder.Ended()
		require.Len(t, ended, 1)
		span := ended[0]
		assert.Equal(t, int64(404), attrInt(span.Attributes(), semconv.HTTPResponseStatusCodeKey))
		assert.Equal(t, codes.Unset, span.Status().Code)
		assert.Empty(t, attrString(span.Attributes(), semconv.ErrorTypeKey))
		assert.Equal(t, "not_found", attrString(span.Attributes(), attribute.Key("app.error_code")))
	})

	t.Run("bare 5xx sets bounded error type", func(t *testing.T) {
		app, recorder := newTracingTestApp(t)
		app.Get("/bare", func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusInternalServerError)
		})

		req, _ := http.NewRequest("GET", "/bare", http.NoBody)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
		_ = resp.Body.Close()

		ended := recorder.Ended()
		require.Len(t, ended, 1)
		assert.Equal(t, codes.Error, ended[0].Status().Code)
		assert.Equal(t, "http_500", attrString(ended[0].Attributes(), semconv.ErrorTypeKey))
	})
}

func TestTracingMiddlewareUnmatchedRouteDoesNotUseRawPath(t *testing.T) {
	app, recorder := newTracingTestApp(t)

	req, _ := http.NewRequest("GET", "/not/registered?email=user@example.com", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_ = resp.Body.Close()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	span := ended[0]
	assert.Equal(t, "GET", span.Name())
	assert.Empty(t, attrString(span.Attributes(), semconv.HTTPRouteKey))
	for _, attr := range span.Attributes() {
		assert.NotContains(t, attr.Value.String(), "/not/registered")
		assert.NotContains(t, attr.Value.String(), "user@example.com")
	}
}

func TestTracingMiddlewareNilProviderIsSafe(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(RequestID())
	app.Use(Tracing(nil, nil, false))
	app.Get("/ok", func(c fiber.Ctx) error { return c.SendString("ok") })

	req, _ := http.NewRequest("GET", "/ok", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestTracingMiddlewareTenantFromContext(t *testing.T) {
	app, recorder := newTracingTestApp(t)
	app.Use(func(c fiber.Ctx) error {
		c.SetContext(tenant.WithID(c.Context(), 5))
		return c.Next()
	})
	app.Get("/tenant", func(c fiber.Ctx) error { return c.SendString("ok") })

	req, _ := http.NewRequest("GET", "/tenant", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, int64(5), attrInt(ended[0].Attributes(), attribute.Key("tenant.id")))
}

func TestLoggerAddsTraceIdentifiers(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	capture := &captureHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previous) })

	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(RequestID())
	app.Use(Tracing(provider, nil, false))
	app.Use(Logger())
	app.Get("/logged", func(c fiber.Ctx) error { return c.SendString("ok") })

	req, _ := http.NewRequest("GET", "/logged", http.NoBody)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	span := ended[0]

	records := capture.snapshot()
	require.NotEmpty(t, records)
	var sawTrace, sawSpan bool
	sawTrace = hasStringAttr(records[0], "trace_id", span.SpanContext().TraceID().String())
	sawSpan = hasStringAttr(records[0], "span_id", span.SpanContext().SpanID().String())
	assert.True(t, sawTrace, "request log must carry the trace_id")
	assert.True(t, sawSpan, "request log must carry the span_id")
}

func TestTraceAttrsEmptyWithoutSpan(t *testing.T) {
	assert.Empty(t, traceAttrs(context.Background()))
}

func attrString(attrs []attribute.KeyValue, key attribute.Key) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.AsString()
		}
	}
	return ""
}

func attrInt(attrs []attribute.KeyValue, key attribute.Key) int64 {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.AsInt64()
		}
	}
	return 0
}

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

func hasStringAttr(record slog.Record, key, want string) bool {
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key && attr.Value.String() == want {
			found = true
			return false
		}
		return true
	})
	return found
}
