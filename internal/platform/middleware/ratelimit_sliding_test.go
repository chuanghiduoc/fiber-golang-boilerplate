package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

// fixedClock returns a clock the test moves by hand, so window boundaries are
// deterministic instead of timing-dependent.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

func (c *fixedClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func slidingTestApp(t *testing.T, limit, window int, clock *fixedClock, shared cache.Cache) *fiber.App {
	t.Helper()
	if shared == nil {
		shared = cache.NewMemoryCache()
		t.Cleanup(func() { _ = shared.Close() })
	}
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/", newLimiter(limit, window, "normal", shared, clock.Now), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	return app
}

func doSlidingRequest(t *testing.T, app *fiber.App) *http.Response {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

// TestSlidingWindowCapsBoundaryBurst is the regression for the fixed window:
// filling the last second of one window used to allow another full limit in
// the first second of the next, so a boundary burst reached 2x the limit.
func TestSlidingWindowCapsBoundaryBurst(t *testing.T) {
	clock := &fixedClock{now: time.Unix(60_000, 0)} // aligned to the window
	app := slidingTestApp(t, 10, 60, clock, nil)

	for i := 0; i < 10; i++ {
		resp := doSlidingRequest(t, app)
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("request %d in the first window = %d, want 200", i+1, resp.StatusCode)
		}
	}

	// Step over the boundary. The previous window still weighs almost 1.0, so
	// the first request of the new window must be rejected.
	clock.Advance(60 * time.Second)
	resp := doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("first request after the boundary = %d, want 429; a boundary burst must stay capped", resp.StatusCode)
	}

	// Half a window later the previous window only contributes half its
	// count, so capacity returns gradually instead of all at once.
	clock.Advance(30 * time.Second)
	resp = doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("request at half-life = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != strconv.Itoa(4) {
		t.Fatalf("X-RateLimit-Remaining at half-life = %q, want 4 (10 - floor(10*0.5) - 1)", got)
	}
}

// TestSlidingWindowLimitOneRecovers is the regression for a weighted estimate
// that counts any positive previous fraction as a whole request: with q=1 the
// first rejected retry used to re-seed the counter, blocking the tier forever.
func TestSlidingWindowLimitOneRecovers(t *testing.T) {
	clock := &fixedClock{now: time.Unix(60_000, 0)}
	app := slidingTestApp(t, 1, 60, clock, nil)

	resp := doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("first request = %d, want 200", resp.StatusCode)
	}

	clock.Advance(60 * time.Second)
	resp = doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("boundary retry = %d, want 429 (the previous request is still in the window)", resp.StatusCode)
	}
	if got := resp.Header.Get(fiber.HeaderRetryAfter); got != "1" {
		t.Fatalf("Retry-After = %q, want 1: capacity returns once the previous count floors to zero", got)
	}

	clock.Advance(time.Second)
	resp = doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("retry at Retry-After = %d, want 200; q=1 must not lock out forever", resp.StatusCode)
	}
}

// TestSlidingWindowRejectedRequestsDoNotPoisonNextWindow proves a throttled
// client's retries are not counted: otherwise hammering during one window
// would become the next window's previous count and extend the block.
func TestSlidingWindowRejectedRequestsDoNotPoisonNextWindow(t *testing.T) {
	clock := &fixedClock{now: time.Unix(60_000, 0)}
	app := slidingTestApp(t, 5, 60, clock, nil)

	for i := 0; i < 5; i++ {
		resp := doSlidingRequest(t, app)
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("request %d = %d, want 200", i+1, resp.StatusCode)
		}
	}
	clock.Advance(60 * time.Second)
	for i := 0; i < 5; i++ {
		resp := doSlidingRequest(t, app)
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusTooManyRequests {
			t.Fatalf("throttled request %d = %d, want 429", i+1, resp.StatusCode)
		}
	}

	// The next window starts from zero current-window usage because every
	// rejected attempt was left out of the counter.
	clock.Advance(60 * time.Second)
	resp := doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("first request of the recovered window = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "4" {
		t.Fatalf("X-RateLimit-Remaining = %q, want a full 4 remaining", got)
	}
}

// ttlRecordingCache captures the TTL the limiter gives a window counter.
type ttlRecordingCache struct {
	cache.Cache
	ttl time.Duration
}

func (c *ttlRecordingCache) IncrementBelow(ctx context.Context, key string, ttl time.Duration, maxValue int64) (value int64, ok bool, err error) {
	c.ttl = ttl
	return c.Cache.IncrementBelow(ctx, key, ttl, maxValue)
}

// TestSlidingWindowCounterTTLCoversNextWindow pins the TTL invariant: a window
// counter is read as the previous counter for the whole following window.
func TestSlidingWindowCounterTTLCoversNextWindow(t *testing.T) {
	clock := &fixedClock{now: time.Unix(60_000, 0)}
	recorder := &ttlRecordingCache{Cache: cache.NewMemoryCache()}
	t.Cleanup(func() { _ = recorder.Close() })
	app := slidingTestApp(t, 5, 60, clock, recorder)

	resp := doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if minTTL := 2 * 60 * time.Second; recorder.ttl < minTTL {
		t.Fatalf("counter TTL = %v, want at least %v so the next window can read it", recorder.ttl, minTTL)
	}
}

// TestSlidingWindowHeaders pins the draft-ietf-httpapi-ratelimit-headers
// format on a fixed clock, so the reset delta is exact.
func TestSlidingWindowHeaders(t *testing.T) {
	clock := &fixedClock{now: time.Unix(60_015, 0)} // 15s into a 60s window
	app := slidingTestApp(t, 5, 60, clock, nil)

	resp := doSlidingRequest(t, app)
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := resp.Header.Get("RateLimit-Policy"), `"normal";q=5;w=60`; got != want {
		t.Errorf("RateLimit-Policy = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("RateLimit"), `"normal";r=4;t=45`; got != want {
		t.Errorf("RateLimit = %q, want %q", got, want)
	}
	if got := resp.Header.Get("X-RateLimit-Reset"); got != "45" {
		t.Errorf("X-RateLimit-Reset = %q, want 45", got)
	}
}
