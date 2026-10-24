package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

const (
	idempotencyHeader            = "Idempotency-Key"
	idempotencyReplayedHeader    = "Idempotent-Replayed"
	idempotencyReplayedLocal     = "idempotency_replayed"
	idempotencyMaxKeyLen         = 128
	idempotencyMaxBodyHashSize   = 1 << 20 // 1 MiB
	idempotencyLockTTL           = 2 * time.Minute
	idempotencyResponseTTL       = 24 * time.Hour
	idempotencyCacheOpTimeout    = 5 * time.Second
	idempotencyBodyMismatchError = "idempotency key was used with a different request body"
)

// cachedResponse is the replay payload persisted under the response key. Only
// status, a small allowlist of headers, a request-body hash, and the response
// body are retained; the request body itself is never stored.
type cachedResponse struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	BodyHash string            `json:"bodyHash,omitempty"`
	Body     []byte            `json:"body,omitempty"`
}

// Idempotency makes mutating routes opt-in idempotent through an
// Idempotency-Key header. The first request claims an atomic cache lock,
// executes the handler, and stores any 2xx response for 24 hours. Retries with
// the same key replay that response; concurrent retries while the first is
// still running get 409. Requests without the header, and GET/HEAD/OPTIONS
// requests, are untouched.
//
// Cache failures fail open: the handler still runs and the client gets the
// normal response, so a cache outage cannot take down the API.
func Idempotency(appCache cache.Cache) fiber.Handler {
	if appCache == nil {
		return func(c fiber.Ctx) error { return c.Next() }
	}

	return func(c fiber.Ctx) error {
		switch c.Method() {
		case fiber.MethodGet, fiber.MethodHead, fiber.MethodOptions:
			return c.Next()
		}

		key := c.Get(idempotencyHeader)
		if key == "" {
			return c.Next()
		}
		if !validIdempotencyKey(key) {
			return apperror.NewBadRequest("invalid Idempotency-Key")
		}

		bodyHash := idempotencyBodyHash(c)
		scope := idempotencyScope(c.Method(), c.Path(), idempotencyIdentity(c), key)
		responseKey := idempotencyResponseKey(scope)
		if cached, ok := loadCachedResponse(c, appCache, responseKey); ok {
			return writeCachedResponse(c, cached, bodyHash)
		}

		lockKey := idempotencyLockKey(scope)
		claimed, err := appCache.Increment(c.Context(), lockKey, idempotencyLockTTL)
		if err != nil {
			// The claim counter is replaced by an owner token once acquired, so
			// a non-integer increment means another request owns the lock (Redis
			// INCR errors; MemoryCache matches that). A real cache outage fails
			// open instead.
			if held, checkErr := idempotencyLockHeld(c, appCache, lockKey); checkErr == nil && held {
				if cached, ok := loadCachedResponse(c, appCache, responseKey); ok {
					return writeCachedResponse(c, cached, bodyHash)
				}
				return apperror.NewConflict("request with this idempotency key is already in progress")
			}
			// The owner may have completed and released the lock between our
			// failed increment and the held check. Re-check the response before
			// failing open, otherwise a completed mutation could execute twice.
			if cached, ok := loadCachedResponse(c, appCache, responseKey); ok {
				return writeCachedResponse(c, cached, bodyHash)
			}
			slog.Warn("idempotency lock failed; proceeding without a lock",
				slog.String("method", c.Method()),
				slog.String("path", c.Path()),
				slog.String("request_id", fiber.Locals[string](c, "request_id")),
				slog.Any("error", err),
			)
			return nextAndCache(c, appCache, responseKey, bodyHash)
		}
		if claimed > 1 {
			// Close the miss->claim->complete race: the owner may have written
			// the response between our first read and the failed claim.
			if cached, ok := loadCachedResponse(c, appCache, responseKey); ok {
				return writeCachedResponse(c, cached, bodyHash)
			}
			return apperror.NewConflict("request with this idempotency key is already in progress")
		}

		// Increment only creates a numeric claim. Replace it with a unique owner
		// token so release can prove ownership and cannot delete a lock that
		// expired and was reacquired by another request.
		token := []byte(uuid.NewString())
		if err := setLockToken(c, appCache, lockKey, token); err != nil {
			slog.Warn("idempotency lock token write failed; proceeding without a lock",
				slog.String("request_id", fiber.Locals[string](c, "request_id")),
				slog.Any("error", err),
			)
			// Release only if the token write actually landed. If it did not,
			// the numeric claim expires after the lock TTL instead of deleting a
			// lock that another request may have acquired in the meantime.
			releaseLock(c, appCache, lockKey, token)
			return nextAndCache(c, appCache, responseKey, bodyHash)
		}

		// The lock must be released even when the handler errors, returns a
		// non-2xx status, or panics. WithoutCancel keeps cleanup independent of
		// a client disconnect; the TTL covers an unreachable cache.
		defer releaseLock(c, appCache, lockKey, token)

		// A request that completed between the first read and the lock claim
		// may have written the response already.
		if cached, ok := loadCachedResponse(c, appCache, responseKey); ok {
			return writeCachedResponse(c, cached, bodyHash)
		}

		return nextAndCache(c, appCache, responseKey, bodyHash)
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

// idempotencyBodyHash binds a key to the request payload for JSON and other
// non-multipart bodies up to 1 MiB. Multipart and larger bodies return an empty
// hash and keep the pre-body-binding semantics. The request body is hashed and
// discarded; only the hash is stored with the cached response.
func idempotencyBodyHash(c fiber.Ctx) string {
	if strings.HasPrefix(strings.ToLower(c.Get(fiber.HeaderContentType)), "multipart/form-data") {
		return ""
	}
	body := c.Body()
	if len(body) > idempotencyMaxBodyHashSize {
		return ""
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
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

func idempotencyLockHeld(c fiber.Ctx, appCache cache.Cache, lockKey string) (bool, error) {
	value, err := appCache.Get(c.Context(), lockKey)
	if err != nil {
		return false, err
	}
	return len(value) > 0, nil
}

func setLockToken(c fiber.Ctx, appCache cache.Cache, lockKey string, token []byte) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), idempotencyCacheOpTimeout)
	defer cancel()
	return appCache.Set(ctx, lockKey, token, idempotencyLockTTL)
}

// releaseLock only deletes the lock when this request still owns it. When the
// optional CompareDeleter capability is unavailable, it falls back to Delete.
func releaseLock(c fiber.Ctx, appCache cache.Cache, lockKey string, token []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), idempotencyCacheOpTimeout)
	defer cancel()

	if deleter, ok := appCache.(cache.CompareDeleter); ok {
		if _, err := deleter.CompareAndDelete(ctx, lockKey, token); err != nil {
			slog.Warn("idempotency lock release failed; it will expire",
				slog.String("request_id", fiber.Locals[string](c, "request_id")),
				slog.Any("error", err),
			)
		}
		return
	}

	if err := appCache.Delete(ctx, lockKey); err != nil {
		slog.Warn("idempotency lock release failed; it will expire",
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Any("error", err),
		)
	}
}

// nextAndCache runs the route and stores its response when it succeeded. The
// original response is never replaced: cache write failures are logged only.
func nextAndCache(c fiber.Ctx, appCache cache.Cache, responseKey, bodyHash string) error {
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
		Status:   status,
		Headers:  headers,
		BodyHash: bodyHash,
		Body:     c.Response().Body(),
	})
	if err != nil {
		slog.Warn("idempotency response encoding failed; not caching",
			slog.String("request_id", fiber.Locals[string](c, "request_id")),
			slog.Any("error", err),
		)
		return nil
	}

	// Use a detached, bounded context so a client disconnect does not abandon a
	// response that later retries need to replay.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), idempotencyCacheOpTimeout)
	defer cancel()
	if err := appCache.Set(ctx, responseKey, payload, idempotencyResponseTTL); err != nil {
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

func writeCachedResponse(c fiber.Ctx, cached *cachedResponse, bodyHash string) error {
	if cached.BodyHash != "" && bodyHash != "" && cached.BodyHash != bodyHash {
		return apperror.NewConflict(idempotencyBodyMismatchError)
	}

	fiber.Locals[bool](c, idempotencyReplayedLocal, true)
	c.Set(idempotencyReplayedHeader, "true")
	c.Status(cached.Status)
	for header, value := range cached.Headers {
		c.Set(header, value)
	}
	return c.Send(cached.Body)
}
