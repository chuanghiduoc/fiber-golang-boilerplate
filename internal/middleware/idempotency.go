package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

const (
	idempotencyHeader      = "Idempotency-Key"
	idempotencyMaxKeyLen   = 128
	idempotencyLockTTL     = 30 * time.Second
	idempotencyResponseTTL = 24 * time.Hour
)

// cachedResponse is the replay payload persisted under the response key. Only
// status, a small allowlist of headers, and the body are retained; request
// credentials and other response headers are never stored.
type cachedResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// Idempotency makes mutating routes opt-in idempotent through an
// Idempotency-Key header. The first request claims an atomic cache lock,
// executes the handler, and stores any 2xx response for 24 hours. Retries with
// the same key replay that response; concurrent retries while the first is
// still running get 409. Requests without the header are untouched.
//
// Cache failures fail open: the handler still runs and the client gets the
// normal response, so a cache outage cannot take down the API.
func Idempotency(appCache cache.Cache) fiber.Handler {
	if appCache == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}

	return func(c fiber.Ctx) error {
		key := c.Get(idempotencyHeader)
		if key == "" {
			return c.Next()
		}
		if !validIdempotencyKey(key) {
			return apperror.NewBadRequest("invalid Idempotency-Key")
		}

		scope := idempotencyScope(c.Method(), c.Path(), idempotencyIdentity(c), key)
		responseKey := idempotencyResponseKey(scope)
		if cached, ok := loadCachedResponse(c, appCache, responseKey); ok {
			return writeCachedResponse(c, cached)
		}

		lockKey := idempotencyLockKey(scope)
		claimed, err := appCache.Increment(c.Context(), lockKey, idempotencyLockTTL)
		if err != nil {
			slog.Warn("idempotency lock failed; proceeding without a lock",
				slog.String("method", c.Method()),
				slog.String("path", c.Path()),
				slog.String("request_id", fiber.Locals[string](c, "request_id")),
				slog.Any("error", err),
			)
			return nextAndCache(c, appCache, responseKey)
		}
		if claimed > 1 {
			return apperror.NewConflict("request with this idempotency key is already in progress")
		}

		// The lock must be released even when the handler errors, returns a
		// non-2xx status, or panics. WithoutCancel keeps cleanup independent of
		// a client disconnect; the short TTL covers an unreachable cache.
		defer func() {
			if err := appCache.Delete(context.WithoutCancel(c.Context()), lockKey); err != nil {
				slog.Warn("idempotency lock release failed; it will expire",
					slog.String("request_id", fiber.Locals[string](c, "request_id")),
					slog.Any("error", err),
				)
			}
		}()

		// A request that completed between the first read and the lock claim
		// may have written the response already.
		if cached, ok := loadCachedResponse(c, appCache, responseKey); ok {
			return writeCachedResponse(c, cached)
		}

		return nextAndCache(c, appCache, responseKey)
	}
}

func validIdempotencyKey(key string) bool {
	if key == "" || len(key) > idempotencyMaxKeyLen {
		return false
	}
	for i := 0; i < len(key); i++ {
		switch ch := key[i]; {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '.', ch == '_', ch == ':', ch == '-':
		default:
			return false
		}
	}
	return true
}

// idempotencyScope isolates keys by method, path, and caller so two users (or
// two endpoints, or two verbs) reusing the same client-generated key cannot
// replay each other's responses. Authenticated routes are scoped by user ID;
// public routes fall back to the client IP.
func idempotencyScope(method, path, identity, key string) string {
	return "idempotency:" + method + ":" + path + ":" + identity + ":" + key
}

func idempotencyIdentity(c fiber.Ctx) string {
	if userID := fiber.Locals[int64](c, "user_id"); userID != 0 {
		return strconv.FormatInt(userID, 10)
	}
	return c.IP()
}

func idempotencyResponseKey(scope string) string { return scope + ":response" }

func idempotencyLockKey(scope string) string { return scope + ":lock" }

// nextAndCache runs the route and stores its response when it succeeded. The
// original response is never replaced: cache write failures are logged only.
func nextAndCache(c fiber.Ctx, appCache cache.Cache, responseKey string) error {
	if err := c.Next(); err != nil {
		return err
	}

	status := c.Response().StatusCode()
	if status < fiber.StatusOK || status >= fiber.StatusMultipleChoices {
		return nil
	}

	headers := make(map[string]string, 3)
	for _, header := range []string{fiber.HeaderContentType, fiber.HeaderLocation, fiber.HeaderETag} {
		if value := c.GetRespHeader(header); value != "" {
			headers[header] = value
		}
	}

	payload, err := json.Marshal(cachedResponse{
		Status:  status,
		Headers: headers,
		Body:    c.Response().Body(),
	})
	if err != nil {
		slog.Warn("idempotency response encoding failed; not caching",
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Any("error", err),
		)
		return nil
	}

	if err := appCache.Set(c.Context(), responseKey, payload, idempotencyResponseTTL); err != nil {
		slog.Warn("idempotency response cache failed; returning original response",
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Any("error", err),
		)
	}
	return nil
}

func loadCachedResponse(c fiber.Ctx, appCache cache.Cache, responseKey string) (*cachedResponse, bool) {
	data, err := appCache.Get(c.Context(), responseKey)
	if err != nil {
		slog.Warn("idempotency cache read failed; proceeding normally",
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Any("error", err),
		)
		return nil, false
	}
	if len(data) == 0 {
		return nil, false
	}

	var cached cachedResponse
	if err := json.Unmarshal(data, &cached); err != nil {
		slog.Warn("idempotency cache entry is invalid; proceeding normally",
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Any("error", err),
		)
		return nil, false
	}
	return &cached, true
}

func writeCachedResponse(c fiber.Ctx, cached *cachedResponse) error {
	c.Status(cached.Status)
	for header, value := range cached.Headers {
		c.Set(header, value)
	}
	return c.Send(cached.Body)
}
