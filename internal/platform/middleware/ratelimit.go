package middleware

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

const (
	rateLimitKeyPrefix = "ratelimit:"
	// rateLimitKeySlack keeps a window's counter alive slightly past the end
	// of the window that follows it, so a request racing a boundary never
	// re-creates the previous window's counter.
	rateLimitKeySlack = time.Second

	headerRateLimitLimit     = "X-RateLimit-Limit"
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRateLimitReset     = "X-RateLimit-Reset"
	// IETF draft-ietf-httpapi-ratelimit-headers fields. They are emitted next
	// to the legacy X-RateLimit-* fields so existing clients keep working.
	headerRateLimitPolicy = "RateLimit-Policy"
	headerRateLimit       = "RateLimit"

	// rateLimitCacheOpTimeout bounds one counter increment or previous-window
	// read. Cache errors fail open, so a slow or blackholed cache must fail
	// fast instead of adding the cache client's dial/read timeouts to every
	// request.
	rateLimitCacheOpTimeout = 250 * time.Millisecond
)

// NewLimiter returns a sliding-window rate limiter backed by atomic cache
// increments (an INCR+PEXPIRE Lua script on Redis, a locked map in memory).
// Every replica sharing the cache increments the same counters, so there is
// no read-then-write race between processes.
//
// The estimate is the standard sliding-window approximation: the current
// window's count plus the previous window's count weighted by how much of it
// still overlaps the sliding window. A client that fills one window therefore
// cannot double its allowance at the next boundary, unlike a fixed window.
//
// A nil counter falls back to a process-local memory cache (tests and
// minimal deployments). Cache errors fail open: the request is allowed, logged,
// and counted in rate_limit_errors_total, matching the idempotency middleware,
// so a cache blip never becomes a 5xx for every API request. Login lockout
// still fails closed in the user service.
func NewLimiter(maxRequests, windowSecs int, prefix string, counter cache.Cache) fiber.Handler {
	return newLimiter(maxRequests, windowSecs, prefix, counter, time.Now)
}

// newLimiter is NewLimiter with an injectable clock, so tests can drive window
// boundaries deterministically.
func newLimiter(maxRequests, windowSecs int, prefix string, counter cache.Cache, now func() time.Time) fiber.Handler {
	if maxRequests < 1 || windowSecs < 1 {
		// Config validation enforces both; reaching this is a programming error.
		panic("middleware: NewLimiter requires maxRequests >= 1 and windowSecs >= 1")
	}
	if counter == nil {
		counter = cache.NewMemoryCache()
	}
	window := int64(windowSecs)
	quoted := strconv.Quote(prefix)
	policy := quoted + ";q=" + strconv.Itoa(maxRequests) + ";w=" + strconv.FormatInt(window, 10)
	limit := strconv.Itoa(maxRequests)

	return func(c fiber.Ctx) error {
		hit, ok := countHit(c, counter, prefix, limiterKey(c, prefix), maxRequests, window, now)
		if !ok {
			return c.Next()
		}

		reset := strconv.FormatInt(hit.resetIn, 10)
		remainingText := strconv.FormatInt(hit.remaining, 10)
		c.Set(headerRateLimitPolicy, policy)
		c.Set(headerRateLimit, quoted+";r="+remainingText+";t="+reset)
		c.Set(headerRateLimitLimit, limit)
		c.Set(headerRateLimitRemaining, remainingText)
		c.Set(headerRateLimitReset, reset)
		if !hit.allowed {
			c.Set(fiber.HeaderRetryAfter, strconv.FormatInt(hit.retryAfter, 10))
			return apperror.NewCode(apperror.CodeRateLimitExceeded, "too many requests, please try again later")
		}

		return c.Next()
	}
}

// NewIPGuard returns a sliding-window, per-client-IP limit check that does not
// call c.Next: it returns nil when the request may proceed and a 429 problem
// otherwise. It is meant to run in front of authentication, where the caller
// is not yet known, so floods of bogus credentials cannot reach the API-key
// store or session checks unthrottled. Windows, the shared cache counters, and
// fail-open behavior match NewLimiter; no rate-limit headers are set, because
// the per-user tier behind authentication reports the caller's quota.
func NewIPGuard(maxRequests, windowSecs int, prefix string, counter cache.Cache) func(fiber.Ctx) error {
	return newIPGuard(maxRequests, windowSecs, prefix, counter, time.Now)
}

// newIPGuard is NewIPGuard with an injectable clock.
func newIPGuard(maxRequests, windowSecs int, prefix string, counter cache.Cache, now func() time.Time) func(fiber.Ctx) error {
	if maxRequests < 1 || windowSecs < 1 {
		// Config validation enforces both; reaching this is a programming error.
		panic("middleware: NewIPGuard requires maxRequests >= 1 and windowSecs >= 1")
	}
	if counter == nil {
		counter = cache.NewMemoryCache()
	}
	window := int64(windowSecs)

	return func(c fiber.Ctx) error {
		hit, ok := countHit(c, counter, prefix, prefix+":ip:"+c.IP(), maxRequests, window, now)
		if !ok || hit.allowed {
			return nil
		}
		c.Set(fiber.HeaderRetryAfter, strconv.FormatInt(hit.retryAfter, 10))
		return apperror.NewCode(apperror.CodeRateLimitExceeded, "too many requests, please try again later")
	}
}

// rateLimitHit is one request's sliding-window decision.
type rateLimitHit struct {
	allowed bool
	// remaining is what is left of the limit after this request, never negative.
	remaining int64
	// resetIn is the seconds until the tier's window boundary. It is the
	// RateLimit;t / X-RateLimit-Reset value (the policy window reset).
	resetIn int64
	// retryAfter is the seconds until a rejected caller can be admitted,
	// accounting for the previous window's decay. It can exceed resetIn by one
	// second because a full current window becomes the previous one.
	retryAfter int64
}

// countHit reads the previous window, derives how many current-window requests
// still fit (the previous count weighted by its remaining overlap, floored),
// and increments the current counter atomically only if it is below that cap.
// A rejected request never advances a counter, so retrying while throttled does
// not extend the block. ok is false when the cache failed; the caller then
// fails open (the failure is logged and counted here).
func countHit(c fiber.Ctx, counter cache.Cache, prefix, bucket string, maxRequests int, window int64, now func() time.Time) (rateLimitHit, bool) {
	nowUnix := now().Unix()
	windowStart := nowUnix - nowUnix%window
	elapsed := nowUnix - windowStart
	resetIn := window - elapsed // 1..window

	// The counter must survive its own window and the whole next window, in
	// which it is read as the previous window's count.
	ttl := time.Duration(2*window)*time.Second + rateLimitKeySlack
	key := rateLimitKeyPrefix + bucket + ":" + strconv.FormatInt(windowStart, 10)
	previousKey := rateLimitKeyPrefix + bucket + ":" + strconv.FormatInt(windowStart-window, 10)

	// One 250ms budget covers every cache operation, so a stalled cache adds
	// at most one bound to the request.
	opCtx, cancel := context.WithTimeout(c.Context(), rateLimitCacheOpTimeout)
	defer cancel()
	raw, err := counter.Get(opCtx, previousKey)
	if err != nil {
		rateLimitCacheFailure(c, prefix, err)
		return rateLimitHit{}, false
	}
	var previous int64
	if len(raw) > 0 {
		previous, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			rateLimitCacheFailure(c, prefix, err)
			return rateLimitHit{}, false
		}
	}

	// Floor the decaying previous count: a fractional contribution must not
	// consume a whole slot, or a limit of 1 would never admit another request.
	weightedPrevious := previous * (window - elapsed) / window
	limit := int64(maxRequests)
	current, allowed, err := counter.IncrementBelow(opCtx, key, ttl, limit-weightedPrevious)
	if err != nil {
		rateLimitCacheFailure(c, prefix, err)
		return rateLimitHit{}, false
	}
	if !allowed {
		return rateLimitHit{
			resetIn:    resetIn,
			retryAfter: admissionDelay(previous, current, limit, elapsed, window),
		}, true
	}
	remaining := limit - weightedPrevious - current
	if remaining < 0 {
		remaining = 0
	}
	return rateLimitHit{allowed: true, remaining: remaining, resetIn: resetIn}, true
}

// admissionDelay returns the first second (1..2*window) at which a request
// would fit, holding the counters constant. It models the current window's
// count becoming the next window's previous count, so Retry-After points at
// real capacity instead of merely the next boundary.
func admissionDelay(previous, current, limit, elapsed, window int64) int64 {
	for delay := int64(1); delay <= 2*window; delay++ {
		offset := (elapsed + delay) / window
		at := (elapsed + delay) % window
		prev, cur := previous, current
		switch offset {
		case 0:
			// Still in the current window.
		case 1:
			prev, cur = current, 0
		default:
			prev, cur = 0, 0
		}
		if prev*(window-at)/window+cur+1 <= limit {
			return delay
		}
	}
	return window
}

func rateLimitCacheFailure(c fiber.Ctx, prefix string, err error) {
	metrics.RateLimitErrorsTotal.WithLabelValues(prefix).Inc()
	slog.WarnContext(c.Context(), "rate-limit counter failed; allowing request",
		slog.String("tier", prefix),
		slog.String("request_id", requestctx.RequestID(c.Context())),
		slog.Any("error", err),
	)
}

// limiterKey namespaces by tier (strict/normal/relaxed) so one bucket cannot
// consume another tier's quota. API keys get their own bucket so rotating or
// revoking one key cannot exhaust another key's quota; authenticated routes
// are otherwise limited per user so clients behind a shared NAT do not
// exhaust each other.
func limiterKey(c fiber.Ctx, prefix string) string {
	if id := apikey.APIKeyIDFromContext(c.Context()); id != uuid.Nil {
		return prefix + ":apikey:" + id.String()
	}
	if id := requestctx.ActorID(c.Context()); id != uuid.Nil {
		return prefix + ":user:" + id.String()
	}
	return prefix + ":ip:" + c.IP()
}
