package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	stressJobCount    = 50
	stressConcurrency = 8
)

// stressStore is a concurrency-safe in-memory store for Worker.Run stress
// tests. Claim removes each job from the pending set, mirroring the database's
// atomic claim, so a duplicate handler or bookkeeping call indicates a worker
// bug rather than a store redelivery.
type stressStore struct {
	mu         sync.Mutex
	pending    []claimedJob
	claimed    []int64
	succeeded  map[int64]int
	failed     map[int64]int
	heartbeats int
	claimCalls int
}

func newStressStore(total int) *stressStore {
	store := &stressStore{
		pending:   make([]claimedJob, 0, total),
		succeeded: make(map[int64]int, total),
		failed:    make(map[int64]int, total),
	}
	for i := 0; i < total; i++ {
		store.pending = append(store.pending, claimedJob{
			ID:          int64(i + 1),
			Kind:        "stress.ok",
			Payload:     json.RawMessage(fmt.Sprintf(`{"seq":%d}`, i)),
			Attempts:    1,
			MaxAttempts: 3,
		})
	}
	return store
}

func (s *stressStore) Claim(_ context.Context, _ string, limit int, _ time.Duration) ([]claimedJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.claimCalls++
	if limit <= 0 || len(s.pending) == 0 {
		return nil, nil
	}
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	batch := make([]claimedJob, limit)
	copy(batch, s.pending[:limit])
	s.pending = s.pending[limit:]
	for _, job := range batch {
		s.claimed = append(s.claimed, job.ID)
	}
	return batch, nil
}

func (s *stressStore) Succeed(_ context.Context, id int64, _ string, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.succeeded[id]++
	return nil
}

func (s *stressStore) Fail(_ context.Context, id int64, _ string, _ int, _ string, _ *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[id]++
	return nil
}

func (s *stressStore) Heartbeat(_ context.Context, _ int64, _ string, _ int, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats++
	return nil
}

// Release is counted as a recorded outcome alongside failures.
func (s *stressStore) Release(_ context.Context, id int64, _ string, _ int, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed[id]++
	return nil
}

func (s *stressStore) PurgeTerminal(_ context.Context, _ time.Duration) (int64, error) {
	return 0, nil
}

type stressSnapshot struct {
	pending    int
	claimed    int
	claimCalls int
	heartbeats int
	succeeded  map[int64]int
	failed     map[int64]int
}

func (s *stressStore) snapshot() stressSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	succeeded := make(map[int64]int, len(s.succeeded))
	for id, count := range s.succeeded {
		succeeded[id] = count
	}
	failed := make(map[int64]int, len(s.failed))
	for id, count := range s.failed {
		failed[id] = count
	}
	return stressSnapshot{
		pending:    len(s.pending),
		claimed:    len(s.claimed),
		claimCalls: s.claimCalls,
		heartbeats: s.heartbeats,
		succeeded:  succeeded,
		failed:     failed,
	}
}

// TestWorkerRunStressProcessesEachJobOnce drives Worker.Run with more jobs than
// slots and asserts the worker never loses, duplicates, or double-records work.
func TestWorkerRunStressProcessesEachJobOnce(t *testing.T) {
	store := newStressStore(stressJobCount)
	worker := NewWorker(nil, "stress-worker",
		WithConcurrency(stressConcurrency),
		WithPollInterval(time.Millisecond),
		WithHeartbeatInterval(time.Millisecond),
		WithLeaseDuration(10*time.Millisecond),
	)
	worker.store = store

	var (
		handledMu sync.Mutex
		handled   = make(map[int64]int, stressJobCount)
		processed atomic.Int32
	)
	started := make(chan struct{}, stressJobCount)
	release := make(chan struct{})
	allHandled := make(chan struct{})

	worker.Register("stress.ok", func(ctx context.Context, payload json.RawMessage) error {
		var item struct {
			Seq int `json:"seq"`
		}
		if err := json.Unmarshal(payload, &item); err != nil {
			return err
		}
		id := int64(item.Seq + 1)

		handledMu.Lock()
		handled[id]++
		handledMu.Unlock()

		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}

		if processed.Add(1) == stressJobCount {
			close(allHandled)
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	for i := 0; i < stressConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d handlers reached the barrier", i, stressConcurrency)
		}
	}

	// Keep the first wave blocked until a heartbeat has run, so heartbeat
	// writes race handler execution and the final success bookkeeping.
	require.Eventually(t, func() bool {
		return store.snapshot().heartbeats > 0
	}, 5*time.Second, time.Millisecond, "handlers must heartbeat while they run")
	close(release)

	select {
	case <-allHandled:
	case <-time.After(10 * time.Second):
		t.Fatalf("only %d/%d jobs were processed", processed.Load(), stressJobCount)
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err, "Run must return nil after cancellation")
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}

	assert.Equal(t, int32(stressJobCount), processed.Load())

	snapshot := store.snapshot()
	assert.Zero(t, snapshot.pending, "every job must be claimed")
	assert.Equal(t, stressJobCount, snapshot.claimed, "each job must be claimed exactly once")
	assert.GreaterOrEqual(t, snapshot.heartbeats, 1)
	assert.Empty(t, snapshot.failed)
	require.Len(t, snapshot.succeeded, stressJobCount)
	for id, count := range snapshot.succeeded {
		assert.Equal(t, 1, count, "job %d must be recorded succeeded exactly once", id)
	}

	handledMu.Lock()
	defer handledMu.Unlock()
	require.Len(t, handled, stressJobCount)
	for id, count := range handled {
		assert.Equal(t, 1, count, "job %d must be handled exactly once", id)
	}
}

// TestWorkerRunStressCancelWhileHandlersRun proves cancellation stops new
// claims, lets in-flight handlers finish, and still records their outcomes
// before Run returns.
func TestWorkerRunStressCancelWhileHandlersRun(t *testing.T) {
	store := newStressStore(stressJobCount)
	worker := NewWorker(nil, "stress-cancel-worker",
		WithConcurrency(stressConcurrency),
		WithPollInterval(time.Millisecond),
	)
	worker.store = store

	started := make(chan struct{}, stressConcurrency)
	worker.Register("stress.ok", func(ctx context.Context, _ json.RawMessage) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	for i := 0; i < stressConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d handlers started", i, stressConcurrency)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err, "Run must return nil after cancellation")
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not wait for in-flight handlers")
	}

	snapshot := store.snapshot()
	assert.Equal(t, stressConcurrency, snapshot.claimed,
		"cancellation must stop further claims")
	assert.Equal(t, stressJobCount-stressConcurrency, snapshot.pending,
		"jobs that were never claimed must remain pending")
	assert.Empty(t, snapshot.succeeded)
	require.Len(t, snapshot.failed, stressConcurrency,
		"every in-flight handler must record its outcome before Run returns")
	for id, count := range snapshot.failed {
		assert.Equal(t, 1, count, "job %d must record one failure", id)
	}
}
