package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

func TestNewLimiter_KeysAPIKeysSeparately(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		switch c.Get("X-Test-Key") {
		case "one":
			fiber.Locals[int64](c, apikey.LocalAPIKeyID, 1)
		case "two":
			fiber.Locals[int64](c, apikey.LocalAPIKeyID, 2)
		}
		return c.Next()
	})
	app.Get("/", NewLimiter(1, 60, "normal", cache.NewMemoryCache()), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	do := func(key string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "/", http.NoBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Test-Key", key)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if status := do("one"); status != fiber.StatusOK {
		t.Fatalf("first request status = %d, want 200", status)
	}
	if status := do("one"); status != fiber.StatusTooManyRequests {
		t.Fatalf("second request for key one status = %d, want 429", status)
	}
	if status := do("two"); status != fiber.StatusOK {
		t.Fatalf("request for key two status = %d, want an independent 200 bucket", status)
	}
}

func TestNewLimiter_HeadersAndProblem(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Get("/", NewLimiter(2, 60, "normal", cache.NewMemoryCache()), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	for want := 1; want >= 0; want-- {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", http.NoBody))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("X-RateLimit-Limit"); got != "2" {
			t.Errorf("X-RateLimit-Limit = %q, want 2", got)
		}
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != strconv.Itoa(want) {
			t.Errorf("X-RateLimit-Remaining = %q, want %d", got, want)
		}
		if reset, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Reset")); err != nil || reset < 1 || reset > 60 {
			t.Errorf("X-RateLimit-Reset = %q, want 1..60", resp.Header.Get("X-RateLimit-Reset"))
		}
	}

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != fiber.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if retry, err := strconv.Atoi(resp.Header.Get(fiber.HeaderRetryAfter)); err != nil || retry < 1 || retry > 60 {
		t.Errorf("Retry-After = %q, want 1..60", resp.Header.Get(fiber.HeaderRetryAfter))
	}
	var problem struct {
		Code   string `json:"code"`
		Status int    `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem: %v", err)
	}
	if problem.Code != string(apperror.CodeRateLimitExceeded) || problem.Status != fiber.StatusTooManyRequests {
		t.Errorf("problem = %+v, want rate-limit code and 429", problem)
	}
	if ct := resp.Header.Get(fiber.HeaderContentType); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
}

// TestNewLimiter_ConcurrentRequestsNeverExceedLimit is the regression for the
// previous GET-then-SET storage: two replicas (two limiter instances here,
// sharing one cache) could both read the same count and both admit a request.
func TestNewLimiter_ConcurrentRequestsNeverExceedLimit(t *testing.T) {
	const (
		limit    = 25
		requests = 200
	)
	shared := cache.NewMemoryCache()
	defer func() { _ = shared.Close() }()

	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	ok := func(c fiber.Ctx) error { return c.SendString("ok") }
	app.Get("/a", NewLimiter(limit, 3600, "normal", shared), ok)
	app.Get("/b", NewLimiter(limit, 3600, "normal", shared), ok)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		path := "/a"
		if i%2 == 1 {
			path = "/b"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, path, http.NoBody))
			if err != nil {
				t.Errorf("app.Test: %v", err)
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == fiber.StatusOK {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	// The hourly window makes a boundary crossing during this sub-second run
	// practically impossible, so the admitted count must be exact.
	if got := allowed.Load(); got != limit {
		t.Fatalf("allowed = %d, want exactly %d", got, limit)
	}
}

type failingCounter struct{ cache.Cache }

func (failingCounter) Increment(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("redis: connection refused")
}

// blackholeCache models an unreachable cache whose calls hang: every
// operation blocks until its context ends (or blackholeMaxWait as a test
// safety net), so callers only return quickly if they bound each call.
type blackholeCache struct{ calls atomic.Int64 }

const blackholeMaxWait = 10 * time.Second

func (b *blackholeCache) wait(ctx context.Context) error {
	b.calls.Add(1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(blackholeMaxWait):
		return errors.New("blackhole: caller did not bound the cache call")
	}
}

func (b *blackholeCache) Get(ctx context.Context, _ string) ([]byte, error) { return nil, b.wait(ctx) }
func (b *blackholeCache) Set(ctx context.Context, _ string, _ []byte, _ time.Duration) error {
	return b.wait(ctx)
}
func (b *blackholeCache) SetNX(ctx context.Context, _ string, _ []byte, _ time.Duration) (bool, error) {
	return false, b.wait(ctx)
}
func (b *blackholeCache) Delete(ctx context.Context, _ string) error { return b.wait(ctx) }
func (b *blackholeCache) Exists(ctx context.Context, _ string) (bool, error) {
	return false, b.wait(ctx)
}
func (b *blackholeCache) Increment(ctx context.Context, _ string, _ time.Duration) (int64, error) {
	return 0, b.wait(ctx)
}
func (b *blackholeCache) Close() error                   { return nil }
func (b *blackholeCache) Ping(ctx context.Context) error { return b.wait(ctx) }

func TestNewLimiter_FailsOpenFastOnHangingCache(t *testing.T) {
	counter := &blackholeCache{}
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Get("/", NewLimiter(1, 60, "normal", counter), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	start := time.Now()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200 (fail open)", resp.StatusCode)
	}
	if counter.calls.Load() != 1 {
		t.Fatalf("cache calls = %d, want 1", counter.calls.Load())
	}
	if elapsed < rateLimitCacheOpTimeout || elapsed > 4*rateLimitCacheOpTimeout {
		t.Fatalf("request took %v, want about the %v per-op bound", elapsed, rateLimitCacheOpTimeout)
	}
}

func TestNewLimiter_FailsOpenOnCacheError(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Get("/", NewLimiter(1, 60, "normal", failingCounter{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	for i := 0; i < 3; i++ {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/", http.NoBody))
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("request %d status = %d, want 200 (fail open)", i, resp.StatusCode)
		}
	}
}
