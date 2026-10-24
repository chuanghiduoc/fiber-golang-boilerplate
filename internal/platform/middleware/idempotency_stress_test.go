package middleware

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

const (
	idempotencyStressRequests = 50
	idempotencyStressPath     = "/stress"
	idempotencyStressTimeout  = 30 * time.Second
)

type stressResponse struct {
	status   int
	body     string
	replayed bool
	err      error
}

func doStressRequest(app *fiber.App, key, body string) stressResponse {
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(http.MethodPost, idempotencyStressPath, reader)
	if err != nil {
		return stressResponse{err: err}
	}
	req.Header.Set("Idempotency-Key", key)
	if body != "" {
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	}

	resp, err := app.Test(req, fiber.TestConfig{
		Timeout:       idempotencyStressTimeout,
		FailOnTimeout: true,
	})
	if err != nil {
		return stressResponse{err: err}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return stressResponse{err: err}
	}
	if err := resp.Body.Close(); err != nil {
		return stressResponse{err: err}
	}
	return stressResponse{
		status:   resp.StatusCode,
		body:     string(data),
		replayed: resp.Header.Get("Idempotent-Replayed") == "true",
	}
}

// startStressRequests launches one request per body behind a shared barrier and
// returns a buffer that receives every response plus a wait function.
func startStressRequests(app *fiber.App, key string, bodies []string) (responses <-chan stressResponse, wait func()) {
	start := make(chan struct{})
	results := make(chan stressResponse, len(bodies))

	var wg sync.WaitGroup
	wg.Add(len(bodies))
	for _, body := range bodies {
		go func(body string) {
			defer wg.Done()
			<-start
			results <- doStressRequest(app, key, body)
		}(body)
	}
	close(start)
	return results, wg.Wait
}

func collectStressResponses(t *testing.T, results <-chan stressResponse, count int) []stressResponse {
	t.Helper()

	responses := make([]stressResponse, 0, count)
	for len(responses) < count {
		select {
		case response := <-results:
			if response.err != nil {
				t.Fatalf("request failed: %v", response.err)
			}
			responses = append(responses, response)
		case <-time.After(15 * time.Second):
			t.Fatalf("received %d/%d responses", len(responses), count)
		}
	}
	return responses
}

// TestIdempotencyStressSingleExecution fires 50 requests with one key while the
// winning handler is blocked. Exactly one handler runs, one response is 2xx,
// and every duplicate fails with a conflict until a later retry replays.
func TestIdempotencyStressSingleExecution(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	const key = "stress-single-key"

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	app := newTestApp()
	app.Post(idempotencyStressPath, Idempotency(mem), func(c fiber.Ctx) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return c.Status(fiber.StatusCreated).SendString("executed")
	})

	results, wait := startStressRequests(app, key, make([]string, idempotencyStressRequests))
	waitDone := make(chan struct{})
	go func() { wait(); close(waitDone) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the winning handler did not start")
	}

	// A duplicate must return conflict while the owner holds the lock, so
	// requestCount-1 responses arrive before the owner is released.
	responses := collectStressResponses(t, results, idempotencyStressRequests-1)
	releaseAll()
	responses = append(responses, collectStressResponses(t, results, 1)...)

	select {
	case <-waitDone:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent requests did not finish")
	}

	created, conflicts := 0, 0
	for _, response := range responses {
		switch response.status {
		case fiber.StatusCreated:
			created++
			if response.body != "executed" {
				t.Errorf("created body = %q, want executed", response.body)
			}
		case fiber.StatusConflict:
			conflicts++
			if !strings.Contains(response.body, "already in progress") {
				t.Errorf("conflict body = %q, want an in-progress conflict", response.body)
			}
		default:
			t.Errorf("unexpected status %d: %s", response.status, response.body)
		}
	}
	if created != 1 || conflicts != idempotencyStressRequests-1 {
		t.Fatalf("statuses = one %d and %d conflicts, want one 201 and %d 409s",
			created, conflicts, idempotencyStressRequests-1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}

	lockKey := idempotencyLockKey(idempotencyScope(
		http.MethodPost, idempotencyStressPath, "0.0.0.0", key))
	exists, err := mem.Exists(context.Background(), lockKey)
	if err != nil {
		t.Fatalf("check lock: %v", err)
	}
	if exists {
		t.Fatal("the lock survived after the owning request finished")
	}

	replay := doStressRequest(app, key, "")
	if replay.err != nil {
		t.Fatalf("replay request: %v", replay.err)
	}
	if replay.status != fiber.StatusCreated || replay.body != "executed" || !replay.replayed {
		t.Fatalf("replay = %d %q replayed=%v, want 201 executed replayed=true",
			replay.status, replay.body, replay.replayed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after replay = %d, want 1", got)
	}
}

// TestIdempotencyStressBodyBinding races 50 different bodies under one key and
// then proves the cached response only replays for the body that won. No body
// variation may execute the handler a second time.
func TestIdempotencyStressBodyBinding(t *testing.T) {
	mem := cache.NewMemoryCache()
	t.Cleanup(func() { _ = mem.Close() })

	const key = "stress-body-key"

	var calls atomic.Int32
	winnerCh := make(chan string, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	app := newTestApp()
	app.Post(idempotencyStressPath, Idempotency(mem), func(c fiber.Ctx) error {
		body := string(c.Body())
		if calls.Add(1) == 1 {
			winnerCh <- body
			close(started)
		}
		<-release
		return c.Status(fiber.StatusCreated).SendString("executed:" + body)
	})

	phaseOneBodies := make([]string, idempotencyStressRequests)
	for i := range phaseOneBodies {
		phaseOneBodies[i] = fmt.Sprintf(`{"phase":1,"seq":%d}`, i)
	}
	results, wait := startStressRequests(app, key, phaseOneBodies)
	waitDone := make(chan struct{})
	go func() { wait(); close(waitDone) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the winning handler did not start")
	}
	winnerBody := <-winnerCh

	responses := collectStressResponses(t, results, idempotencyStressRequests-1)
	releaseAll()
	responses = append(responses, collectStressResponses(t, results, 1)...)

	select {
	case <-waitDone:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent body-variation requests did not finish")
	}

	created, conflicts := 0, 0
	for _, response := range responses {
		switch response.status {
		case fiber.StatusCreated:
			created++
		case fiber.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d: %s", response.status, response.body)
		}
	}
	if created != 1 || conflicts != idempotencyStressRequests-1 {
		t.Fatalf("phase one statuses = %d created, %d conflicts; want 1 and %d",
			created, conflicts, idempotencyStressRequests-1)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after phase one = %d, want 1", got)
	}

	// The cached response now binds the key to winnerBody. Every concurrent
	// request with a different body must be rejected without running the
	// handler, regardless of request interleaving.
	mismatchBodies := make([]string, idempotencyStressRequests)
	for i := range mismatchBodies {
		mismatchBodies[i] = fmt.Sprintf(`{"phase":2,"seq":%d}`, i)
	}
	mismatchResults, mismatchWait := startStressRequests(app, key, mismatchBodies)
	mismatchResponses := collectStressResponses(t, mismatchResults, len(mismatchBodies))
	mismatchWait()

	for _, response := range mismatchResponses {
		if response.status != fiber.StatusConflict {
			t.Errorf("different body status = %d, want 409: %s", response.status, response.body)
		}
		if !strings.Contains(response.body, "different request body") {
			t.Errorf("different body response = %q, want a body-mismatch conflict", response.body)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after body mismatches = %d, want 1", got)
	}

	// Concurrent retries with the winning body are replays, not executions.
	replayBodies := make([]string, idempotencyStressRequests)
	for i := range replayBodies {
		replayBodies[i] = winnerBody
	}
	replayResults, replayWait := startStressRequests(app, key, replayBodies)
	replayResponses := collectStressResponses(t, replayResults, len(replayBodies))
	replayWait()

	for _, response := range replayResponses {
		if response.status != fiber.StatusCreated {
			t.Errorf("replay status = %d, want 201: %s", response.status, response.body)
		}
		if response.body != "executed:"+winnerBody {
			t.Errorf("replay body = %q, want %q", response.body, "executed:"+winnerBody)
		}
		if !response.replayed {
			t.Error("replay response must be marked Idempotent-Replayed")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after replays = %d, want 1", got)
	}
}
