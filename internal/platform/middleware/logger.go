package middleware

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/trace"

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

func Logger() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()

		err := c.Next()

		latency := time.Since(start)
		status := effectiveStatus(c, err)

		var userID int64
		if v := fiber.Locals[int64](c, "user_id"); v != 0 {
			userID = v
		}

		attrs := make([]slog.Attr, 0, 13)
		attrs = append(attrs,
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Duration("latency", latency),
			slog.String("ip", c.IP()),
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Int64("user_id", userID),
			slog.Int("content_length", c.Request().Header.ContentLength()),
			slog.String("user_agent", c.Get("User-Agent")),
			slog.String("query", redactQuery(string(c.Request().URI().QueryString()))),
		)
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
