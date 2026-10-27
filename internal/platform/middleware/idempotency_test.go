package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

var errStubCache = errors.New("stub cache failure")

// failingCache returns errors for every operation so tests can prove the
// middleware fails open instead of turning a cache outage into a 5xx.
type failingCache struct{}

func (failingCache) Get(context.Context, string) ([]byte, error) { return nil, errStubCache }
func (failingCache) Set(context.Context, string, []byte, time.Duration) error {
	return errStubCache
}
func (failingCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, errStubCache
}
func (failingCache) Delete(context.Context, string) error         { return errStubCache }
func (failingCache) Exists(context.Context, string) (bool, error) { return false, errStubCache }
func (failingCache) Increment(context.Context, string, time.Duration) (int64, error) {
	return 0, errStubCache
}
func (failingCache) IncrementBelow(context.Context, string, time.Duration, int64) (value int64, ok bool, err error) {
	return 0, false, errStubCache
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

func (r *recordingCache) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	r.record(key)
	return r.Cache.SetNX(ctx, key, value, ttl)
}

func (r *recordingCache) Delete(ctx context.Context, key string) error {
	r.record(key)
	return r.Cache.Delete(ctx, key)
}

func doIdempotencyRequest(t *testing.T, app *fiber.App, method, path, key, body string) (resp *http.Response, responseBody string) {
	t.Helper()

	contentType := ""
	if body != "" {
		contentType = fiber.MIMEApplicationJSON
	}
	return doIdempotencyRequestWithContentType(t, app, method, path, key, body, contentType)
}

func doIdempotencyRequestWithContentType(
	t *testing.T, app *fiber.App, method, path, key, body, contentType string,
) (resp *http.Response, responseBody string) {
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
	if contentType != "" {
		req.Header.Set(fiber.HeaderContentType, contentType)
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

func multipartBody(t *testing.T, field, value string) (body, contentType string) {
	t.Helper()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField(field, value); err != nil {
		t.Fatalf("write multipart field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf.String(), writer.FormDataContentType()
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
	if first.Header.Get("Idempotent-Replayed") != "" {
		t.Errorf("first response should not be marked replayed")
	}
	if got := second.Header.Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("replay marker = %q, want true", got)
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

func TestIdempotency_BodyBindingRejectsDifferentPayload(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	})

	const firstBody = `{"name":"alice"}`
	first, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "body-key", firstBody)
	if first.StatusCode != fiber.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.StatusCode)
	}

	conflict, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", "body-key", `{"name":"bob"}`)
	if conflict.StatusCode != fiber.StatusUnprocessableEntity {
		t.Fatalf("different body status = %d, want 422; body = %s", conflict.StatusCode, body)
	}
	if !strings.Contains(body, "different request body") || !strings.Contains(body, `"code":"idempotency.key_mismatch"`) {
		t.Errorf("different body response = %s, want a body-mismatch conflict", body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after body mismatch = %d, want 1", got)
	}

	replay, replayBody := doIdempotencyRequest(t, app, http.MethodPost, "/test", "body-key", firstBody)
	if replay.StatusCode != fiber.StatusCreated || replayBody != "call-1" {
		t.Fatalf("same body replay = %d %q, want 201 call-1", replay.StatusCode, replayBody)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after same-body replay = %d, want 1", got)
	}
}

func TestIdempotency_MultipartSkipsBodyBinding(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	})

	firstBody, firstType := multipartBody(t, "name", "alice")
	secondBody, secondType := multipartBody(t, "name", "bob")

	first, _ := doIdempotencyRequestWithContentType(t, app, http.MethodPost, "/test", "multipart-key", firstBody, firstType)
	if first.StatusCode != fiber.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.StatusCode)
	}

	second, body := doIdempotencyRequestWithContentType(t, app, http.MethodPost, "/test", "multipart-key", secondBody, secondType)
	if second.StatusCode != fiber.StatusCreated || body != "call-1" {
		t.Fatalf("multipart replay = %d %q, want 201 call-1", second.StatusCode, body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (multipart bodies skip binding)", got)
	}
}

func TestIdempotency_LargeBodySkipsBodyBinding(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(mem), func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	})

	firstBody := strings.Repeat("a", idempotencyMaxBodyHashSize+1)
	secondBody := strings.Repeat("b", idempotencyMaxBodyHashSize+1)

	first, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "large-key", firstBody)
	if first.StatusCode != fiber.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.StatusCode)
	}

	second, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", "large-key", secondBody)
	if second.StatusCode != fiber.StatusCreated || body != "call-1" {
		t.Fatalf("large-body replay = %d %q, want 201 call-1", second.StatusCode, body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (large bodies skip binding)", got)
	}
}

func TestIdempotency_ReadMethodsPassThrough(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.All("/read", Idempotency(mem), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.SendString("ok")
	})

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		resp, body := doIdempotencyRequest(t, app, method, "/read", "read-key", "")
		if resp.StatusCode != fiber.StatusOK {
			t.Errorf("%s status = %d, want 200; body = %s", method, resp.StatusCode, body)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("handler calls = %d, want 3 (GET/HEAD/OPTIONS are pass-through)", got)
	}
}

func TestIdempotency_SlowHandlerSingleExecution(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	const (
		key         = "slow-key"
		concurrency = 5
	)
	lockKey := idempotencyLockKey(idempotencyScope(http.MethodPost, "/slow", uuid.Nil, "", "0.0.0.0", key))

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	lockValues := make(chan []byte, 1)

	app := newTestApp()
	app.Post("/slow", Idempotency(mem), func(c fiber.Ctx) error {
		if calls.Add(1) == 1 {
			value, _ := mem.Get(context.Background(), lockKey)
			lockValues <- value
			close(started)
		}
		<-release
		return c.Status(fiber.StatusCreated).SendString("done")
	})

	type result struct {
		status int
		body   string
		err    error
	}
	do := func() result {
		req, err := http.NewRequest(http.MethodPost, "/slow", http.NoBody)
		if err != nil {
			return result{err: err}
		}
		req.Header.Set("Idempotency-Key", key)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
		if err != nil {
			return result{err: err}
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return result{err: err}
		}
		if err := resp.Body.Close(); err != nil {
			return result{err: err}
		}
		return result{status: resp.StatusCode, body: string(data)}
	}

	firstCh := make(chan result, 1)
	go func() { firstCh <- do() }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first handler did not start")
	}

	if lockValue := <-lockValues; string(lockValue) == "1" || len(lockValue) == 0 {
		t.Fatalf("lock value = %q, want a unique owner token", lockValue)
	}

	duplicates := make(chan result, concurrency-1)
	var wg sync.WaitGroup
	for i := 0; i < concurrency-1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			duplicates <- do()
		}()
	}

	duplicatesDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(duplicatesDone)
	}()
	select {
	case <-duplicatesDone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("duplicate requests blocked; the handler likely ran more than once")
	}
	close(release)

	first := <-firstCh
	if first.err != nil {
		t.Fatalf("first request: %v", first.err)
	}
	if first.status != fiber.StatusCreated || first.body != "done" {
		t.Fatalf("first response = %d %q, want 201 done", first.status, first.body)
	}

	statuses := []int{first.status}
	for i := 0; i < concurrency-1; i++ {
		res := <-duplicates
		if res.err != nil {
			t.Fatalf("duplicate request: %v", res.err)
		}
		statuses = append(statuses, res.status)
	}

	created, conflicts := 0, 0
	for _, status := range statuses {
		switch status {
		case fiber.StatusCreated:
			created++
		case fiber.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", status)
		}
	}
	if created != 1 || conflicts != concurrency-1 {
		t.Fatalf("statuses = %v, want one 201 and %d 409s", statuses, concurrency-1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}

	exists, err := mem.Exists(context.Background(), lockKey)
	if err != nil {
		t.Fatalf("check lock: %v", err)
	}
	if exists {
		t.Fatal("lock survived after the owning request finished")
	}
}

// tailWindowCache misses the first response read, then reports a completed
// response after the duplicate fails to claim the lock. It reproduces the
// miss->claim->complete race the middleware closes before returning 409.
type tailWindowCache struct {
	cache.Cache
	response []byte
	gets     atomic.Int32
}

func (c *tailWindowCache) Get(ctx context.Context, key string) ([]byte, error) {
	if strings.HasSuffix(key, ":response") {
		if c.gets.Add(1) == 1 {
			return nil, nil
		}
		return c.response, nil
	}
	return c.Cache.Get(ctx, key)
}

func (c *tailWindowCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, nil
}

func TestIdempotency_TailWindowReplaysCompletedResponse(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	const body = `{"value":1}`
	sum := sha256.Sum256([]byte(body))
	payload, err := json.Marshal(cachedResponse{
		Status:   fiber.StatusCreated,
		Headers:  map[string]string{fiber.HeaderContentType: "application/json"},
		BodyHash: hex.EncodeToString(sum[:]),
		Body:     []byte(`{"cached":true}`),
	})
	if err != nil {
		t.Fatalf("marshal cached response: %v", err)
	}

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(&tailWindowCache{Cache: mem, response: payload}), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.SendString("should not run")
	})

	resp, responseBody := doIdempotencyRequest(t, app, http.MethodPost, "/test", "tail-key", body)
	if resp.StatusCode != fiber.StatusCreated || responseBody != `{"cached":true}` {
		t.Fatalf("response = %d %q, want 201 cached body", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("Idempotent-Replayed"); got != "true" {
		t.Errorf("replay marker = %q, want true", got)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("handler calls = %d, want 0", got)
	}
}

// lostReplyCache models a claim whose reply never arrives: SetNX optionally
// applies the write, then reports a timeout as a network round trip would.
type lostReplyCache struct {
	cache.Cache
	applies bool
}

func (c *lostReplyCache) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if c.applies {
		if _, err := c.Cache.SetNX(ctx, key, value, ttl); err != nil {
			return false, err
		}
	}
	return false, context.DeadlineExceeded
}

// A claim that landed but whose reply timed out is our own lock: the handler
// must run once, a retry must replay it, and the lock must not be left behind
// (previously every retry got 409 for the whole lock TTL).
func TestIdempotency_ClaimAppliedButReplyLostRunsOnce(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(&lostReplyCache{Cache: mem, applies: true}), func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	})

	first, firstBody := doIdempotencyRequest(t, app, http.MethodPost, "/test", "lost-reply", "")
	retry, retryBody := doIdempotencyRequest(t, app, http.MethodPost, "/test", "lost-reply", "")

	if first.StatusCode != fiber.StatusCreated || firstBody != "call-1" {
		t.Fatalf("first = %d %q, want 201 call-1", first.StatusCode, firstBody)
	}
	if retry.StatusCode != fiber.StatusCreated || retryBody != "call-1" || retry.Header.Get(idempotencyReplayedHeader) != "true" {
		t.Fatalf("retry = %d %q replayed=%q, want a 201 replay of call-1",
			retry.StatusCode, retryBody, retry.Header.Get(idempotencyReplayedHeader))
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	lockKey := idempotencyLockKey(idempotencyScope(http.MethodPost, "/test", uuid.Nil, "", "0.0.0.0", "lost-reply"))
	if held, _ := mem.Exists(context.Background(), lockKey); held {
		t.Fatal("lock survived after the owning request finished")
	}
}

// A lost reply while another request holds the lock must still conflict.
func TestIdempotency_ClaimReplyLostWhileHeldConflicts(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })
	lockKey := idempotencyLockKey(idempotencyScope(http.MethodPost, "/test", uuid.Nil, "", "0.0.0.0", "held-key"))
	if err := mem.Set(context.Background(), lockKey, []byte("other-owner"), 30*time.Second); err != nil {
		t.Fatalf("prime lock: %v", err)
	}

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(&lostReplyCache{Cache: mem}), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.SendString("should not run")
	})

	resp, body := doIdempotencyRequest(t, app, http.MethodPost, "/test", "held-key", "")
	if resp.StatusCode != fiber.StatusConflict || calls.Load() != 0 {
		t.Fatalf("status = %d, calls = %d; want 409 and 0; body = %s", resp.StatusCode, calls.Load(), body)
	}
	if owner, _ := mem.Get(context.Background(), lockKey); string(owner) != "other-owner" {
		t.Fatalf("lock owner = %q, want other-owner untouched", owner)
	}
}

func TestIdempotency_ConcurrentClaimConflict(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	lockKey := idempotencyLockKey(idempotencyScope(http.MethodPost, "/test", uuid.Nil, "", "0.0.0.0", "busy-key"))
	if err := mem.Set(context.Background(), lockKey, []byte("other-owner"), 30*time.Second); err != nil {
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

func TestIdempotency_NormalizesEquivalentPaths(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	app := newTestApp()
	app.Post("/users/:id/ban", Idempotency(mem), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("banned-" + c.Params("id"))
	})

	// Case-insensitive, non-strict routing sends all of these to one route and
	// resource, so they must share one idempotency scope.
	for _, path := range []string{"/users/7/ban", "/Users/7/Ban", "/users/7/ban/"} {
		resp, body := doIdempotencyRequest(t, app, http.MethodPost, path, "same-key", "")
		if resp.StatusCode != fiber.StatusCreated || body != "banned-7" {
			t.Fatalf("POST %s = %d %q, want 201 banned-7", path, resp.StatusCode, body)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (equivalent paths must replay)", got)
	}

	// A different parameter value is a different resource.
	if resp, body := doIdempotencyRequest(t, app, http.MethodPost, "/users/8/ban", "same-key", ""); resp.StatusCode != fiber.StatusCreated || body != "banned-8" {
		t.Fatalf("different id = %d %q, want 201 banned-8", resp.StatusCode, body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after id change = %d, want 2", got)
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
			c.SetContext(requestctx.WithActorID(c.Context(), testID(int(id))))
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

func TestIdempotency_ScopesByTenant(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	var calls atomic.Int32
	tenantLocals := func(c fiber.Ctx) error {
		// Simulate the global tenant middleware: the same caller IP and
		// Idempotency-Key must not collide across tenants.
		switch c.Get("X-Test-Tenant") {
		case "acme":
			c.SetContext(tenant.WithID(c.Context(), testID(2)))
			c.SetContext(tenant.WithSlug(c.Context(), "acme"))
		default:
			c.SetContext(tenant.WithID(c.Context(), testID(1)))
			c.SetContext(tenant.WithSlug(c.Context(), "default"))
		}
		return c.Next()
	}
	app := newTestApp()
	app.Post("/register", tenantLocals, Idempotency(mem), func(c fiber.Ctx) error {
		n := calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("call-" + strconv.Itoa(int(n)))
	})

	do := func(tenant string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"email":"shared@example.com"}`))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Idempotency-Key", "shared-key")
		req.Header.Set("Content-Type", fiber.MIMEApplicationJSON)
		req.Header.Set("X-Test-Tenant", tenant)
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

	first, firstBody := do("default")
	if first.StatusCode != fiber.StatusCreated || firstBody != "call-1" {
		t.Fatalf("default tenant = %d %q, want 201 call-1", first.StatusCode, firstBody)
	}
	second, secondBody := do("acme")
	if second.StatusCode != fiber.StatusCreated || secondBody != "call-2" {
		t.Fatalf("acme tenant = %d %q, want 201 call-2", second.StatusCode, secondBody)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want 2 (tenants must not share a replay)", got)
	}

	// A replay inside the same tenant still returns the original response.
	replay, replayBody := do("acme")
	if replay.StatusCode != fiber.StatusCreated || replayBody != "call-2" {
		t.Fatalf("same-tenant replay = %d %q, want 201 call-2", replay.StatusCode, replayBody)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after replay = %d, want 2", got)
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

func TestIdempotency_HangingCacheFailsOpenFast(t *testing.T) {
	appCache := &blackholeCache{}
	var calls atomic.Int32
	app := newTestApp()
	app.Post("/test", Idempotency(appCache), func(c fiber.Ctx) error {
		calls.Add(1)
		return c.Status(fiber.StatusCreated).SendString("ok")
	})

	req, err := http.NewRequest(http.MethodPost, "/test", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set(idempotencyHeader, "cache-hangs")
	start := time.Now()
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != fiber.StatusCreated || calls.Load() != 1 {
		t.Fatalf("status = %d, handler calls = %d; want 201 and 1 (fail open)", resp.StatusCode, calls.Load())
	}
	// Every call is bounded: lookup, claim, claim read-back, and re-lookup use
	// the read bound; storing the response and releasing the lock use the
	// write bound.
	budget := 4*idempotencyCacheReadTimeout + IdempotencyPostHandlerBudget
	if elapsed > budget+time.Second {
		t.Fatalf("request took %v with a hanging cache, want at most about %v", elapsed, budget)
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
	body := `{"password":"` + password + `"}`
	resp, _ := doIdempotencyRequest(t, app, http.MethodPost, "/test", "safe-key", body)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	sum := sha256.Sum256([]byte(body))
	bodyHash := hex.EncodeToString(sum[:])

	keys := recorder.recordedKeys()
	if len(keys) == 0 {
		t.Fatal("no cache keys were recorded")
	}
	for _, key := range keys {
		for _, secret := range []string{"password", password, bodyHash} {
			if strings.Contains(key, secret) {
				t.Errorf("cache key %q contains request data %q", key, secret)
			}
		}
		if !strings.HasPrefix(key, "idempotency:POST:/test:tenant="+uuid.Nil.String()+"::0.0.0.0:") {
			t.Errorf("cache key %q is not scoped by method, path, tenant, and identity", key)
		}
	}
}
