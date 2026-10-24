package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

var errStubCache = errors.New("stub cache failure")

// failingCache returns errors for every operation so tests can prove the
// middleware fails open instead of turning a cache outage into a 5xx.
type failingCache struct{}

func (failingCache) Get(context.Context, string) ([]byte, error) { return nil, errStubCache }
func (failingCache) Set(context.Context, string, []byte, time.Duration) error {
	return errStubCache
}
func (failingCache) Delete(context.Context, string) error         { return errStubCache }
func (failingCache) Exists(context.Context, string) (bool, error) { return false, errStubCache }
func (failingCache) Increment(context.Context, string, time.Duration) (int64, error) {
	return 0, errStubCache
}
func (failingCache) Close() error               { return nil }
func (failingCache) Ping(context.Context) error { return errStubCache }

// recordingCache captures generated keys so tests can assert that request
// bodies and credentials never become part of a cache key.
type recordingCache struct {
	cache.Cache
	mu   sync.Mutex
	keys []string
}

func (r *recordingCache) record(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, key)
}

func (r *recordingCache) recordedKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.keys...)
}

func (r *recordingCache) Get(ctx context.Context, key string) ([]byte, error) {
	r.record(key)
	return r.Cache.Get(ctx, key)
}

func (r *recordingCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	r.record(key)
	return r.Cache.Set(ctx, key, value, ttl)
}

func (r *recordingCache) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	r.record(key)
	return r.Cache.Increment(ctx, key, ttl)
}

func (r *recordingCache) Delete(ctx context.Context, key string) error {
	r.record(key)
	return r.Cache.Delete(ctx, key)
}

func doIdempotencyRequest(t *testing.T, app *fiber.App, method, path, key, body string) (resp *http.Response, responseBody string) {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != "" {
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}

	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	return resp, string(data)
}

func TestIdempotency_NoHeaderRunsEveryTime(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	})

	first, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "", "")
	second, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "", "")

	if first.StatusCode != fiber.StatusCreated || second.StatusCode != fiber.StatusCreated {
		t.Fatalf("statuses = %d, %d; want 201, 201", first.StatusCode, second.StatusCode)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 without an Idempotency-Key", got)
	}
}

func TestIdempotency_ReplaysCachedResponse(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		calls.Add(1)
		c.Set(fiber.HeaderContentType, "application/json")
		c.Set(fiber.HeaderLocation, "/test/1")
		c.Set(fiber.HeaderETag, `"v1"`)
		return c.Status(fiber.StatusCreated).SendString(`{"id":1}`)
	})

	first, firstBody := doIdempotencyRequest(t, app, http.MethodPost, "/test", "replay-key", "")
	second, secondBody := doIdempotencyRequest(t, app, http.MethodPost, "/test", "replay-key", "")

	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	if second.StatusCode != first.StatusCode {
		t.Fatalf("replay status = %d, want %d", second.StatusCode, first.StatusCode)
	}
	if secondBody != firstBody {
		t.Fatalf("replay body = %q, want %q", secondBody, firstBody)
	}
	for _, header := range []string{fiber.HeaderContentType, fiber.HeaderLocation, fiber.HeaderETag} {
		if got, want := second.Header.Get(header), first.Header.Get(header); got != want {
			t.Errorf("replay header %s = %q, want %q", header, got, want)
		}
	}
}

func TestIdempotency_ConcurrentClaimConflict(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	lockKey := idempotencyLockKey(idempotencyScope(http.MethodPost, "/test", "0.0.0.0", "busy-key"))
	if _, err := mem.Increment(context.Background(), lockKey, 30*time.Second); err != nil {
		t.Fatalf("prime lock: %v", err)
	}

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.SendString("should not run")
	})

	resp, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", "busy-key", "")

	if resp.StatusCode != fiber.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "already in progress") {
		t.Errorf("body = %s, want an in-progress conflict", body)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0 while another request holds the lock", got)
	}
}

func TestIdempotency_InvalidKey(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.SendString("should not run")
	})

	for _, key := range []string{"has space", "bad/slash", strings.Repeat("a", 129)} {
		resp, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", key, "")
		if resp.StatusCode != fiber.StatusBadRequest {
			t.Errorf("key %q: status = %d, want 400; body = %s", key, resp.StatusCode, body)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0 for rejected keys", got)
	}
}

func TestIdempotency_HandlerErrorNotCachedAndLockReleased(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		if calls.Add(1) == 1 {
			return apperror.NewInternal("boom")
		}
		return c.Status(fiber.StatusCreated).SendString("recovered")
	})

	first, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "error-key", "")
	if first.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", first.StatusCode)
	}

	second, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", "error-key", "")
	if second.StatusCode != fiber.StatusCreated {
		t.Fatalf("second status = %d, want 201; body = %s", second.StatusCode, body)
	}
	if body != "recovered" {
		t.Fatalf("second body = %q, want recovered", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 (error must not be cached)", got)
	}
}

func TestIdempotency_Non2xxNotCachedAndLockReleased(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		if calls.Add(1) == 1 {
			return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": "invalid"})
		}
		return c.Status(fiber.StatusCreated).SendString("created")
	})

	first, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "bad-status-key", "")
	if first.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("first status = %d, want 400", first.StatusCode)
	}

	second, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", "bad-status-key", "")
	if second.StatusCode != fiber.StatusCreated {
		t.Fatalf("second status = %d, want 201; body = %s", second.StatusCode, body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 (non-2xx must not be cached)", got)
	}
}

func TestIdempotency_ScopesByUserPathAndMethod(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	userLocals := func(c fiber.Ctx) error {
		if raw := c.Get("X-Test-User"); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return err
			}
			fiber.Locals[int64](c, "user_id", id)
		}
		return c.Next()
	}
	handler := func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	}

	app := newTestApp()
	app.Post("/test", userLocals, Idempotency(mem), handler)
	app.Put("/test", userLocals, Idempotency(mem), handler)
	app.Post("/other", userLocals, Idempotency(mem), handler)

	postWithUser := func(user string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "/test", http.NoBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Idempotency-Key", "shared-key")
		req.Header.Set("X-Test-User", user)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		_ = resp.Body.Close()
		return resp, string(data)
	}

	if _, body := postWithUser("1"); body != "call-1" {
		t.Fatalf("first user body = %q, want call-1", body)
	}
	if _, body := postWithUser("2"); body != "call-2" {
		t.Fatalf("different user body = %q, want call-2", body)
	}
	// Same user, path, method, and key must replay rather than run again.
	if resp, body := postWithUser("1"); resp.StatusCode != fiber.StatusCreated || body != "call-1" {
		t.Fatalf("replay = %d %q, want 201 call-1", resp.StatusCode, body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after replay = %d, want 2", got)
	}

	// A different path with the same key must not collide.
	if resp, _ := doIdempotencyRequest(t, app, http.MethodPost, "/other", "shared-key", ""); resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("different path status = %d, want 201", resp.StatusCode)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler calls after path change = %d, want 3", got)
	}

	// A different method with the same key must not collide.
	if resp, _ := doIdempotencyRequest(t, app, http.MethodPut, "/test", "shared-key", ""); resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("different method status = %d, want 201", resp.StatusCode)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("handler calls after method change = %d, want 4", got)
	}
}

func TestIdempotency_CacheFailureFailsOpen(t *testing.T) {
	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(failingCache{}), func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	})

	first, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "cache-down", "")
	second, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", "cache-down", "")

	if first.StatusCode != fiber.StatusCreated || second.StatusCode != fiber.StatusCreated {
		t.Fatalf("statuses = %d, %d; want 201, 201", first.StatusCode, second.StatusCode)
	}
	if body != "call-2" {
		t.Fatalf("second body = %q, want call-2", body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 when the cache fails", got)
	}
}

func TestIdempotency_NilCacheFailsOpen(t *testing.T) {
	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(nil), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("ok")
	})

	for i := 0; i < 2; i++ {
		resp, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "nil-cache", "")
		if resp.StatusCode != fiber.StatusCreated {
			t.Fatalf("request %d: status = %d, want 201", i, resp.StatusCode)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 with no cache", got)
	}
}

func TestIdempotency_KeysDoNotContainBodyOrPassword(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })
	recorder := &recordingCache{Cache: mem}

	app := newTestApp()
	app.Post("/test", Idempotency(recorder), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	const password = "super-secret-password"
	resp, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "safe-key", `{"password":"`+password+`"}`)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	keys := recorder.recordedKeys()
	if len(keys) == 0 {
		t.Fatal("no cache keys were recorded")
	}
	for _, key := range keys {
		for _, secret := range []string{"password", password} {
			if strings.Contains(key, secret) {
				t.Errorf("cache key %q contains request secret %q", key, secret)
			}
		}
		if !strings.HasPrefix(key, "idempotency:POST:/test:0.0.0.0:") {
			t.Errorf("cache key %q is not scoped by method, path, and identity", key)
		}
	}
}
