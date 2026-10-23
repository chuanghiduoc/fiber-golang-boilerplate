package middleware

import (
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
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
		status := c.Response().StatusCode()

		var userID int64
		if v := fiber.Locals[int64](c, "user_id"); v != 0 {
			userID = v
		}

		attrs := []slog.Attr{
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
		}

		switch {
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
