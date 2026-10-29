package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const (
	idempotencyHeader         = "Idempotency-Key"
	idempotencyReplayedHeader = "Idempotent-Replayed"
	idempotencyReplayedLocal  = "idempotency_replayed"
	idempotencyMaxKeyLen      = 128
	// idempotencyBodyMemoryLimit keeps ordinary JSON bodies in memory while
	// spooling larger uploads to a temporary file instead of duplicating them on
	// the heap.
	idempotencyBodyMemoryLimit   = 64 << 10
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
// idempotency.key_mismatch. The scope includes the resolved tenant so identical
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
		bodyHash, cleanup, err := idempotencyBodyHash(c)
		if err != nil {
			return err
		}
		defer cleanup()
		req := idempotentRequest{
			c:           c,
			cache:       appCache,
			responseKey: idempotencyResponseKey(scope),
			lockKey:     idempotencyLockKey(scope),
			bodyHash:    bodyHash,
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
		slog.WarnContext(r.c.Context(), "idempotency lock unavailable; proceeding without a lock",
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
		slog.WarnContext(r.c.Context(), "idempotency lock claim failed",
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
		slog.WarnContext(r.c.Context(), "idempotency lock release failed; it will expire",
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
		slog.WarnContext(r.c.Context(), "idempotency response encoding failed; not caching",
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
		slog.WarnContext(r.c.Context(), "idempotency response cache failed; returning original response",
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
		slog.WarnContext(r.c.Context(), "idempotency cache read failed; proceeding normally",
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
		slog.WarnContext(r.c.Context(), "idempotency cache entry is invalid; proceeding normally",
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
	return requestctx.RequestID(r.c.Context())
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

// idempotencyBodyHash binds a key to the exact request payload, including
// multipart uploads and bodies larger than the JSON limit. The route's body
// limit bounds this read. Small buffered bodies stay in memory; a streamed body
// is copied to a private temporary file and the file is put back for Huma and
// the handler. This avoids copying an entire multipart upload into the heap
// merely to compute the digest. The caller must run cleanup on every path; it
// closes and removes the file after the handler finished with the body.
func idempotencyBodyHash(c fiber.Ctx) (digest string, cleanup func(), err error) {
	stream := c.Request().BodyStream()
	if stream == nil {
		body := c.BodyRaw()
		// humafiber reads Request().BodyStream() when StreamRequestBody is
		// enabled, so a buffered body must be exposed as a stream too.
		c.Request().SetBodyStream(bytes.NewReader(body), len(body))
		return hashRequestBody(body), func() {}, nil
	}

	limit := idempotencyRequestLimit(c)
	readLimit := int64(idempotencyBodyMemoryLimit) + 1
	if limit+1 < readLimit {
		readLimit = limit + 1
	}
	prefix, readErr := io.ReadAll(io.LimitReader(stream, readLimit))
	if readErr != nil {
		return "", nil, apperror.NewBadRequest("failed to read request body")
	}
	if int64(len(prefix)) > limit {
		return "", nil, apperror.NewPayloadTooLarge("request body too large")
	}
	if int64(len(prefix)) < readLimit {
		// The stream ended within the in-memory threshold. Keep it exposed as
		// a stream because that is what humafiber reads when streaming is on.
		c.Request().SetBodyStream(bytes.NewReader(prefix), len(prefix))
		return hashRequestBody(prefix), func() {}, nil
	}

	tmp, createErr := os.CreateTemp("", "fiber-idempotency-body-*")
	if createErr != nil {
		return "", nil, apperror.Internal(createErr, "failed to buffer request body")
	}
	cleanup = func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	prefixWritten, writeErr := tmp.Write(prefix)
	if writeErr != nil {
		cleanup()
		return "", nil, apperror.Internal(writeErr, "failed to buffer request body")
	}
	if prefixWritten != len(prefix) {
		cleanup()
		return "", nil, apperror.Internal(io.ErrShortWrite, "failed to buffer request body")
	}

	hasher := sha256.New()
	_, _ = hasher.Write(prefix)
	remaining := limit - int64(len(prefix))
	written, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(stream, remaining+1))
	total := int64(len(prefix)) + written
	if copyErr != nil {
		cleanup()
		return "", nil, apperror.NewBadRequest("failed to read request body")
	}
	if total > limit {
		cleanup()
		return "", nil, apperror.NewPayloadTooLarge("request body too large")
	}
	if _, seekErr := tmp.Seek(0, io.SeekStart); seekErr != nil {
		cleanup()
		return "", nil, apperror.Internal(seekErr, "failed to buffer request body")
	}
	c.Request().SetBodyStream(tmp, int(total))
	return hex.EncodeToString(hasher.Sum(nil)), cleanup, nil
}

// idempotencyRequestLimit is the ceiling already configured on the Fiber app.
// BodyLimit rejects declared oversize bodies before this middleware runs; this
// check also bounds a chunked multipart upload so hashing cannot spool an
// unbounded body to disk.
func idempotencyRequestLimit(c fiber.Ctx) int64 {
	limit := c.App().Config().BodyLimit
	if limit <= 0 {
		limit = fiber.DefaultBodyLimit
	}
	return int64(limit)
}

func hashRequestBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// idempotencyScope isolates keys by method, path, tenant, and caller so two
// users (or two endpoints, two verbs, or two tenants sharing an IP) reusing the
// same client-generated key cannot replay each other's responses. The tenant ID
// is the security boundary; the slug is included for operator readability.
// Authenticated routes are scoped by user ID; public routes fall back to the
// client IP.
func idempotencyScope(method, path string, tenantID uuid.UUID, tenantSlug, identity, key string) string {
	return "idempotency:" + method + ":" + path +
		":tenant=" + tenantID.String() + ":" + tenantSlug +
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
	if userID := requestctx.ActorID(c.Context()); userID != uuid.Nil {
		return userID.String()
	}
	return c.IP()
}

func idempotencyResponseKey(scope string) string { return scope + ":response" }

func idempotencyLockKey(scope string) string { return scope + ":lock" }
