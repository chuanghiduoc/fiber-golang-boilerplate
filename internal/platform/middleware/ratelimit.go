package middleware

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

const (
	rateLimitKeyPrefix = "ratelimit:"
	// rateLimitKeySlack keeps a window's counter alive slightly past the
	// window end so a request racing the boundary never re-creates it.
	rateLimitKeySlack = time.Second

	headerRateLimitLimit     = "X-RateLimit-Limit"
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRateLimitReset     = "X-RateLimit-Reset"
)

// NewLimiter returns a fixed-window rate limiter backed by the atomic
// cache.Increment (an INCR+PEXPIRE Lua script on Redis, a locked map in
// memory). Every replica sharing the cache increments the same counter, so
// there is no read-then-write race between processes.
//
// Windows are aligned to epoch multiples of windowSecs and the window start is
// part of the key, so a counter can never be reset or extended by a later
// request. Like any fixed window, a client can send up to 2*maxRequests across
// a boundary.
//
// A nil counter falls back to a process-local memory cache (tests and
// minimal deployments). Cache errors fail open: the request is allowed, logged,
// and counted in rate_limit_errors_total, matching the idempotency middleware,
// so a cache blip never becomes a 5xx for every API request. Login lockout
// still fails closed in the user service.
func NewLimiter(maxRequests, windowSecs int, prefix string, counter cache.Cache) fiber.Handler {
	if maxRequests < 1 || windowSecs < 1 {
		// Config validation enforces both; reaching this is a programming error.
		panic("middleware: NewLimiter requires maxRequests >= 1 and windowSecs >= 1")
	}
	if counter == nil {
		counter = cache.NewMemoryCache()
	}
	window := int64(windowSecs)
	limit := strconv.Itoa(maxRequests)

	return func(c fiber.Ctx) error {
		now := time.Now().Unix()
		windowStart := now - now%window
		resetIn := windowStart + window - now // 1..windowSecs

		key := rateLimitKeyPrefix + limiterKey(c, prefix) + ":" + strconv.FormatInt(windowStart, 10)
		hits, err := counter.Increment(c.Context(), key, time.Duration(resetIn)*time.Second+rateLimitKeySlack)
		if err != nil {
			metrics.RateLimitErrorsTotal.WithLabelValues(prefix).Inc()
			slog.Warn("rate-limit counter failed; allowing request",
				slog.String("tier", prefix),
				slog.String("request_id", fiber.Locals[string](c, "request_id")),
				slog.Any("error", err),
			)
			return c.Next()
		}

		reset := strconv.FormatInt(resetIn, 10)
		if hits > int64(maxRequests) {
			c.Set(fiber.HeaderRetryAfter, reset)
			return apperror.NewCode(apperror.CodeRateLimitExceeded, "too many requests, please try again later")
		}

		handlerErr := c.Next()
		c.Set(headerRateLimitLimit, limit)
		c.Set(headerRateLimitRemaining, strconv.FormatInt(int64(maxRequests)-hits, 10))
		c.Set(headerRateLimitReset, reset)
		return handlerErr
	}
}

// limiterKey namespaces by tier (strict/normal/relaxed) so one bucket cannot
// consume another tier's quota. API keys get their own bucket so rotating or
// revoking one key cannot exhaust another key's quota; authenticated routes
// are otherwise limited per user so clients behind a shared NAT do not
// exhaust each other.
func limiterKey(c fiber.Ctx, prefix string) string {
	if id := fiber.Locals[int64](c, apikey.LocalAPIKeyID); id != 0 {
		return prefix + ":apikey:" + strconv.FormatInt(id, 10)
	}
	if id := fiber.Locals[int64](c, "user_id"); id != 0 {
		return prefix + ":user:" + strconv.FormatInt(id, 10)
	}
	return prefix + ":ip:" + c.IP()
}
