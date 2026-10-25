package middleware

import (
	"bytes"
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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
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
	idempotencyBodyMismatchError = "This Idempotency-Key was already used with a different request body. Use a new key for a new request."
	idempotencyInProgressError   = "request with this idempotency key is already in progress"

	// idempotencyCacheReadTimeout bounds each cache call made before the
	// handler runs (lookup, claim, claim read-back). These fail open, so a slow
	// cache must not stall the request for the client's full timeouts.
	idempotencyCacheReadTimeout = 250 * time.Millisecond
	// idempotencyCacheWriteTimeout bounds storing the response and releasing
	// the lock after the handler ran. It is longer than the read bound because
	// losing the stored response lets a retry repeat a completed mutation.
	idempotencyCacheWriteTimeout = 500 * time.Millisecond

	// IdempotencyPostHandlerBudget is the longest this middleware keeps a
	// request running after its handler returned: storing the response, then
	// releasing the lock, each on a detached context.
	IdempotencyPostHandlerBudget = 2 * idempotencyCacheWriteTimeout
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

// lockClaim is the outcome of trying to take the per-key execution lock.
type lockClaim int

const (
	// lockAcquired: this request owns the lock and may run the handler.
	lockAcquired lockClaim = iota
	// lockHeldByOther: another request owns the lock; this one must not run.
	lockHeldByOther
	// lockUnavailable: the cache could not answer; the request fails open.
	lockUnavailable
)

// Idempotency makes mutating routes opt-in idempotent through an
// Idempotency-Key header. The first request claims an atomic owner-token lock,
// executes the handler, and stores any 2xx response for 24 hours. Retries with
// the same key replay that response; concurrent retries while the first is
// still running get 409; reusing a key with a different body gets 422
// idempotency.key_mismatch (ADR 0008). The scope includes the resolved tenant so identical
// keys from two tenants never share a replay. Requests without the header, and
// GET/HEAD/OPTIONS requests, are untouched.
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

		scope := idempotencyScope(
			c.Method(), idempotencyRoutePath(c), tenanthttp.ID(c), tenanthttp.Slug(c), idempotencyIdentity(c), key,
		)
		req := idempotentRequest{
			c:           c,
			cache:       appCache,
			responseKey: idempotencyResponseKey(scope),
			lockKey:     idempotencyLockKey(scope),
			bodyHash:    idempotencyBodyHash(c),
		}
		return req.serve()
	}
}

// idempotentRequest is one keyed mutating request and its cache keys.
type idempotentRequest struct {
	c           fiber.Ctx
	cache       cache.Cache
	responseKey string
	lockKey     string
	bodyHash    string
}

// serve replays a stored response when one exists; otherwise it claims the
// lock and runs the handler at most once per key.
func (r idempotentRequest) serve() error {
	if cached, ok := r.loadResponse(); ok {
		return r.replay(cached)
	}

	token := []byte(uuid.NewString())
	switch r.claimLock(token) {
	case lockHeldByOther:
		// Close the miss->claim->complete race: the owner may have written the
		// response between our first read and the failed claim.
		if cached, ok := r.loadResponse(); ok {
			return r.replay(cached)
		}
		return apperror.NewConflict(idempotencyInProgressError)
	case lockUnavailable:
		slog.Warn("idempotency lock unavailable; proceeding without a lock",
			slog.String("method", r.c.Method()),
			slog.String("path", r.c.Path()),
			slog.String("request_id", r.requestID()),
		)
	case lockAcquired:
	}

	// Release on every exit path: handler error, non-2xx, or panic. It also
	// runs after a fail-open claim, because a claim whose reply was lost may
	// still land; compare-and-delete only ever removes this request's token.
	defer r.releaseLock(token)

	// A request that completed between the first read and the claim may have
	// written the response already.
	if cached, ok := r.loadResponse(); ok {
		return r.replay(cached)
	}
	return r.runAndStore()
}

// claimLock takes the lock with one atomic SET NX carrying this request's
// owner token. When the reply is lost (timeout, connection reset) the SET may
// still have been applied, so the key is read back: our token means we hold
// the lock, another token means someone else does, and a missing key or a
// failed read means the cache cannot arbitrate and the request fails open.
func (r idempotentRequest) claimLock(token []byte) lockClaim {
	ctx, cancel := context.WithTimeout(r.c.Context(), idempotencyCacheReadTimeout)
	stored, err := r.cache.SetNX(ctx, r.lockKey, token, idempotencyLockTTL)
	cancel()
	if err == nil {
		if stored {
			return lockAcquired
		}
		return lockHeldByOther
	}

	ctx, cancel = context.WithTimeout(r.c.Context(), idempotencyCacheReadTimeout)
	defer cancel()
	owner, readErr := r.cache.Get(ctx, r.lockKey)
	switch {
	case readErr != nil || len(owner) == 0:
		slog.Warn("idempotency lock claim failed",
			slog.String("request_id", r.requestID()),
			slog.Any("error", err),
			slog.Any("read_back_error", readErr),
		)
		return lockUnavailable
	case bytes.Equal(owner, token):
		return lockAcquired
	default:
		return lockHeldByOther
	}
}

// releaseLock only deletes the lock when this request still owns it. When the
// optional CompareDeleter capability is unavailable, it falls back to Delete.
func (r idempotentRequest) releaseLock(token []byte) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.c.Context()), idempotencyCacheWriteTimeout)
	defer cancel()

	var err error
	if deleter, ok := r.cache.(cache.CompareDeleter); ok {
		_, err = deleter.CompareAndDelete(ctx, r.lockKey, token)
	} else {
		err = r.cache.Delete(ctx, r.lockKey)
	}
	if err != nil {
		slog.Warn("idempotency lock release failed; it will expire",
			slog.String("request_id", r.requestID()),
			slog.Any("error", err),
		)
	}
}

// runAndStore runs the route and stores its response when it succeeded. The
// original response is never replaced: cache write failures are logged only.
func (r idempotentRequest) runAndStore() error {
	if err := r.c.Next(); err != nil {
		return err
	}

	status := r.c.Response().StatusCode()
	if status < fiber.StatusOK || status >= fiber.StatusMultipleChoices {
		return nil
	}

	headers := make(map[string]string, 3)
	for _, header := range []string{fiber.HeaderContentType, fiber.HeaderLocation, fiber.HeaderETag} {
		if value := r.c.GetRespHeader(header); value != "" {
			headers[header] = value
		}
	}

	payload, err := json.Marshal(cachedResponse{
		Status:   status,
		Headers:  headers,
		BodyHash: r.bodyHash,
		Body:     r.c.Response().Body(),
	})
	if err != nil {
		slog.Warn("idempotency response encoding failed; not caching",
			slog.String("request_id", r.requestID()),
			slog.Any("error", err),
		)
		return nil
	}

	// Use a detached, bounded context so a client disconnect does not abandon a
	// response that later retries need to replay.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.c.Context()), idempotencyCacheWriteTimeout)
	defer cancel()
	if err := r.cache.Set(ctx, r.responseKey, payload, idempotencyResponseTTL); err != nil {
		slog.Warn("idempotency response cache failed; returning original response",
			slog.String("request_id", r.requestID()),
			slog.Any("error", err),
		)
	}
	return nil
}

func (r idempotentRequest) loadResponse() (*cachedResponse, bool) {
	ctx, cancel := context.WithTimeout(r.c.Context(), idempotencyCacheReadTimeout)
	defer cancel()
	data, err := r.cache.Get(ctx, r.responseKey)
	if err != nil {
		slog.Warn("idempotency cache read failed; proceeding normally",
			slog.String("request_id", r.requestID()),
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
			slog.String("request_id", r.requestID()),
			slog.Any("error", err),
		)
		return nil, false
	}
	return &cached, true
}

// replay writes a stored response, unless the key is being reused with a
// different request body.
func (r idempotentRequest) replay(cached *cachedResponse) error {
	if cached.BodyHash != "" && r.bodyHash != "" && cached.BodyHash != r.bodyHash {
		return apperror.NewCode(apperror.CodeIdempotencyKeyMismatch, idempotencyBodyMismatchError).
			WithDetailKey("errors.idempotency.key_mismatch.detail", nil)
	}

	fiber.Locals[bool](r.c, idempotencyReplayedLocal, true)
	r.c.Set(idempotencyReplayedHeader, "true")
	r.c.Status(cached.Status)
	for header, value := range cached.Headers {
		r.c.Set(header, value)
	}
	return r.c.Send(cached.Body)
}

func (r idempotentRequest) requestID() string {
	return fiber.Locals[string](r.c, "request_id")
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

// idempotencyScope isolates keys by method, path, tenant, and caller so two
// users (or two endpoints, two verbs, or two tenants sharing an IP) reusing the
// same client-generated key cannot replay each other's responses. The tenant ID
// is the security boundary; the slug is included for operator readability.
// Authenticated routes are scoped by user ID; public routes fall back to the
// client IP.
func idempotencyScope(method, path string, tenantID int64, tenantSlug, identity, key string) string {
	return "idempotency:" + method + ":" + path +
		":tenant=" + strconv.FormatInt(tenantID, 10) + ":" + tenantSlug +
		":" + identity + ":" + key
}

// idempotencyRoutePath identifies the target resource as the matched route
// template plus its parameter values. Routing is case-insensitive and
// non-strict, so /Users/7/ and /users/7 hit the same handler; the raw path
// would give them different scopes and let a retry run the mutation twice.
// Parameter values keep their case because they may be case-sensitive IDs.
func idempotencyRoutePath(c fiber.Ctx) string {
	route := c.Route()
	if route == nil || route.Path == "" {
		return c.Path()
	}
	var b strings.Builder
	b.WriteString(route.Path)
	for _, name := range route.Params {
		// Length-prefix each value so a value containing the separator cannot
		// collide with a different parameter split.
		value := c.Params(name)
		b.WriteString("|")
		b.WriteString(name)
		b.WriteString("=")
		b.WriteString(strconv.Itoa(len(value)))
		b.WriteString(":")
		b.WriteString(value)
	}
	return b.String()
}

func idempotencyIdentity(c fiber.Ctx) string {
	if userID := fiber.Locals[int64](c, "user_id"); userID != 0 {
		return strconv.FormatInt(userID, 10)
	}
	return c.IP()
}

func idempotencyResponseKey(scope string) string { return scope + ":response" }

func idempotencyLockKey(scope string) string { return scope + ":lock" }
