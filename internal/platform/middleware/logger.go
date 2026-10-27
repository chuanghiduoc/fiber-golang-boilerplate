package middleware

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// sensitiveQueryKeys are redacted before a request URL is written to logs.
// OAuth callbacks and future token-in-query flows must not leak credentials
// into centralized logging.
func sensitiveQueryKey(key string) bool {
	k := strings.ToLower(key)
	return k == "code" || k == "state" || k == "password" ||
		strings.Contains(k, "token") || strings.Contains(k, "secret")
}

func redactQuery(raw string) string {
	if raw == "" {
		return ""
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return "[unparseable]"
	}
	for key := range values {
		if sensitiveQueryKey(key) {
			values.Set(key, "[REDACTED]")
		}
	}
	return values.Encode()
}

func Logger(catalog *apperror.Catalog) fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		latency := time.Since(start)
		status := effectiveStatus(catalog, c, err)

		var userID uuid.UUID
		if v := requestctx.ActorID(c.Context()); v != uuid.Nil {
			userID = v
		}

		attrs := make([]slog.Attr, 0, 14)
		attrs = append(attrs,
			slog.String("http.request.method", httpRequestMethod(c.Method())),
			slog.String("url.path", c.Path()),
			slog.String("http.route", routeTemplate(c)),
			slog.Int("http.response.status_code", status),
			slog.Duration("duration", latency),
			slog.String("client.address", c.IP()),
			slog.String("app.request_id", requestctx.RequestID(c.Context())),
			slog.String("user_agent.original", c.Get("User-Agent")),
			slog.String("url.query", redactQuery(string(c.Request().URI().QueryString()))),
		)
		if size := c.Request().Header.ContentLength(); size >= 0 {
			attrs = append(attrs, slog.Int64("http.request.body.size", int64(size)))
		}
		if userID != uuid.Nil {
			attrs = append(attrs, slog.String(string(semconv.EnduserIDKey), userID.String()))
		}
		attrs = append(attrs, traceAttrs(c.Context())...)

		switch {
		case status == apperror.StatusClientClosedRequest:
			// A client disconnect is expected noise, not a request failure.
			slog.LogAttrs(c.Context(), slog.LevelInfo, "request", attrs...)
		case status >= 500:
			slog.LogAttrs(c.Context(), slog.LevelError, "request", attrs...)
		case status >= 400:
			slog.LogAttrs(c.Context(), slog.LevelWarn, "request", attrs...)
		default:
			slog.LogAttrs(c.Context(), slog.LevelInfo, "request", attrs...)
		}

		return err
	}
}

// traceAttrs exposes the active span identifiers on request logs so a log
// line, trace, and audit event can be correlated. Trace IDs are omitted when
// tracing is disabled or the span is not recording.
func traceAttrs(ctx context.Context) []slog.Attr {
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return nil
	}
	return []slog.Attr{
		slog.String("trace_id", spanCtx.TraceID().String()),
		slog.String("span_id", spanCtx.SpanID().String()),
	}
}
