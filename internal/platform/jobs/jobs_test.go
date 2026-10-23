package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

func TestJobNormalize(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	t.Run("applies defaults", func(t *testing.T) {
		t.Parallel()

		job, err := Job{Kind: "email.send"}.normalize(now)
		require.NoError(t, err)
		assert.Equal(t, "email.send", job.Kind)
		assert.JSONEq(t, `{}`, string(job.Payload))
		assert.Equal(t, now, job.RunAt)
		assert.Equal(t, DefaultMaxAttempts, job.MaxAttempts)
	})

	t.Run("preserves explicit values", func(t *testing.T) {
		t.Parallel()

		runAt := now.Add(time.Hour)
		job, err := Job{
			Kind:        "email.send",
			Payload:     json.RawMessage(`{"to":"user@example.com"}`),
			RunAt:       runAt,
			MaxAttempts: 3,
		}.normalize(now)
		require.NoError(t, err)
		assert.JSONEq(t, `{"to":"user@example.com"}`, string(job.Payload))
		assert.Equal(t, runAt, job.RunAt)
		assert.Equal(t, 3, job.MaxAttempts)
	})

	t.Run("rejects empty kind", func(t *testing.T) {
		t.Parallel()

		_, err := Job{}.normalize(now)
		require.ErrorIs(t, err, ErrEmptyKind)

		_, err = Job{Kind: " \t "}.normalize(now)
		require.ErrorIs(t, err, ErrEmptyKind)
	})

	t.Run("rejects invalid payload", func(t *testing.T) {
		t.Parallel()

		_, err := Job{Kind: "email.send", Payload: json.RawMessage(`{`)}.normalize(now)
		require.ErrorIs(t, err, ErrInvalidPayload)
	})

	t.Run("rejects negative attempts", func(t *testing.T) {
		t.Parallel()

		_, err := Job{Kind: "email.send", MaxAttempts: -1}.normalize(now)
		require.ErrorIs(t, err, ErrInvalidMaxAttempts)
	})
}

func TestScaledBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		attempt int
		base    time.Duration
		max     time.Duration
		want    time.Duration
	}{
		{name: "zero attempts behaves like first", attempt: 0, base: time.Second, max: time.Hour, want: time.Second},
		{name: "first attempt", attempt: 1, base: time.Second, max: time.Hour, want: time.Second},
		{name: "second attempt", attempt: 2, base: time.Second, max: time.Hour, want: 2 * time.Second},
		{name: "third attempt", attempt: 3, base: time.Second, max: time.Hour, want: 4 * time.Second},
		{name: "caps at max", attempt: 100, base: time.Second, max: time.Hour, want: time.Hour},
		{name: "caps small max", attempt: 4, base: 10 * time.Millisecond, max: 40 * time.Millisecond, want: 40 * time.Millisecond},
		{name: "base above max", attempt: 1, base: time.Minute, max: time.Second, want: time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, scaledBackoff(tt.attempt, tt.base, tt.max))
		})
	}

	assert.Equal(t, time.Hour, backoffDelay(64), "default backoff must stay capped")
}

func TestProcessJobSucceeds(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake
	worker.Register("ok", func(_ context.Context, payload json.RawMessage) error {
		assert.JSONEq(t, `{"value":1}`, string(payload))
		return nil
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          7,
		Kind:        "ok",
		Payload:     json.RawMessage(`{"value":1}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	assert.Equal(t, []int64{7}, fake.succeededIDs())
	assert.Empty(t, fake.failures)
}

func TestProcessJobRetriesWithBackoff(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake
	worker.retryBase = 10 * time.Millisecond
	worker.retryMax = time.Second
	worker.Register("boom", func(context.Context, json.RawMessage) error {
		return errors.New("temporary failure")
	})

	before := time.Now()
	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          8,
		Kind:        "boom",
		Payload:     json.RawMessage(`{}`),
		Attempts:    2,
		MaxAttempts: 5,
	}))

	require.Len(t, fake.failures, 1)
	failure := fake.failures[0]
	assert.Equal(t, int64(8), failure.id)
	assert.Equal(t, "test-worker", failure.workerID)
	assert.Contains(t, failure.reason, "temporary failure")
	require.NotNil(t, failure.retryAt)
	// Second attempt backs off 2 * retryBase.
	assert.WithinDuration(t, before.Add(20*time.Millisecond), *failure.retryAt, 100*time.Millisecond)
}

func TestProcessJobMarksDeadAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake
	worker.Register("boom", func(context.Context, json.RawMessage) error {
		return errors.New("permanent failure")
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          9,
		Kind:        "boom",
		Payload:     json.RawMessage(`{}`),
		Attempts:    3,
		MaxAttempts: 3,
	}))

	require.Len(t, fake.failures, 1)
	assert.Nil(t, fake.failures[0].retryAt, "exhausted job must be marked dead")
	assert.Contains(t, fake.failures[0].reason, "permanent failure")
}

func TestProcessJobRecoversPanic(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake
	worker.Register("panic", func(context.Context, json.RawMessage) error {
		panic("kaboom")
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          10,
		Kind:        "panic",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	require.Len(t, fake.failures, 1)
	assert.Contains(t, fake.failures[0].reason, "handler panic")
	assert.Contains(t, fake.failures[0].reason, "kaboom")
	assert.NotNil(t, fake.failures[0].retryAt, "a panic before the limit should be retried")
}

func TestProcessJobUnknownKindIsDead(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          11,
		Kind:        "missing",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 5,
	}))

	require.Len(t, fake.failures, 1)
	assert.Contains(t, fake.failures[0].reason, ErrNoHandler.Error())
	assert.Nil(t, fake.failures[0].retryAt, "an unregistered kind cannot succeed on retry")
}

func TestWorkerRunProcessesAndStops(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{claimed: []claimedJob{{
		ID:          12,
		Kind:        "ok",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}}}
	worker := NewWorker(nil, "test-worker", WithPollInterval(10*time.Millisecond))
	worker.store = fake

	var processed atomic.Int32
	handled := make(chan struct{})
	worker.Register("ok", func(context.Context, json.RawMessage) error {
		processed.Add(1)
		close(handled)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not process the job")
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	assert.Equal(t, int32(1), processed.Load())
	assert.Equal(t, []int64{12}, fake.succeededIDs())
	assert.GreaterOrEqual(t, fake.claimCalls, 1)
}

func TestWorkerRunReturnsImmediatelyWhenCanceled(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, worker.Run(ctx))
	assert.Equal(t, 0, fake.claimCalls)
}

func TestWorkerRunReturnsClaimError(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{claimErr: errors.New("database unavailable")}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake

	err := worker.Run(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database unavailable")
}

func TestWorkerOptions(t *testing.T) {
	t.Parallel()

	worker := NewWorker(nil, "",
		WithPollInterval(0),
		WithConcurrency(0),
		WithHeartbeatInterval(0),
		WithLeaseDuration(-time.Second),
	)
	assert.Equal(t, defaultPollInterval, worker.pollInterval)
	assert.Equal(t, defaultConcurrency, worker.concurrency)
	assert.Equal(t, defaultHeartbeatInterval, worker.heartbeatInterval)
	assert.Equal(t, defaultLeaseDuration, worker.leaseDuration)
	assert.Equal(t, "worker", worker.id)

	worker = NewWorker(nil, "w",
		WithPollInterval(25*time.Millisecond),
		WithConcurrency(4),
		WithHeartbeatInterval(2*time.Second),
		WithLeaseDuration(20*time.Second),
	)
	assert.Equal(t, 25*time.Millisecond, worker.pollInterval)
	assert.Equal(t, 4, worker.concurrency)
	assert.Equal(t, 2*time.Second, worker.heartbeatInterval)
	assert.Equal(t, 20*time.Second, worker.leaseDuration)

	// A heartbeat at or beyond the lease boundary would race expiry, so the
	// interval is reduced to a quarter of the lease.
	worker = NewWorker(nil, "w",
		WithHeartbeatInterval(time.Minute),
		WithLeaseDuration(time.Minute),
	)
	assert.Equal(t, 15*time.Second, worker.heartbeatInterval)
	assert.Equal(t, time.Minute, worker.leaseDuration)

	// An interval that is below the lease but too close to it is still
	// clamped to a quarter of the lease.
	worker = NewWorker(nil, "w",
		WithHeartbeatInterval(4*time.Minute),
		WithLeaseDuration(5*time.Minute),
	)
	assert.Equal(t, 75*time.Second, worker.heartbeatInterval)
	assert.Equal(t, 5*time.Minute, worker.leaseDuration)
}

func TestWorkerRunPassesLeaseDurationToClaim(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{claimed: []claimedJob{{
		ID:          13,
		Kind:        "ok",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}}}
	worker := NewWorker(nil, "test-worker",
		WithPollInterval(time.Millisecond),
		WithLeaseDuration(2*time.Minute),
	)
	worker.store = fake

	handled := make(chan struct{})
	worker.Register("ok", func(context.Context, json.RawMessage) error {
		close(handled)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not process the job")
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	assert.Equal(t, 2*time.Minute, fake.staleAfterSeen())
}

func TestWorkerRunProcessesMoreThanConcurrencyAndStops(t *testing.T) {
	t.Parallel()

	const (
		total       = 7
		concurrency = 2
	)
	pending := make([]claimedJob, total)
	for i := range pending {
		pending[i] = claimedJob{
			ID:          int64(i + 1),
			Kind:        "ok",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}
	}
	fake := &fakeStore{}
	worker := NewWorker(nil, "stream-worker",
		WithConcurrency(concurrency),
		WithPollInterval(time.Millisecond),
	)
	worker.store = &streamingStore{fakeStore: fake, pending: pending}

	handled := make(chan struct{}, total)
	worker.Register("ok", func(context.Context, json.RawMessage) error {
		handled <- struct{}{}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	for i := 0; i < total; i++ {
		select {
		case <-handled:
		case <-time.After(3 * time.Second):
			t.Fatalf("processed %d/%d jobs before timeout", i, total)
		}
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop; finished-channel send likely blocked")
	}

	require.Eventually(t, func() bool {
		return len(fake.succeededIDs()) == total
	}, 2*time.Second, 10*time.Millisecond)
	assert.GreaterOrEqual(t, fake.claimCalls, 1)
}

func TestProcessJobHeartbeatsWhileHandlerRuns(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "heartbeat-worker",
		WithHeartbeatInterval(5*time.Millisecond),
		WithLeaseDuration(250*time.Millisecond),
	)
	worker.store = fake
	worker.Register("slow", func(context.Context, json.RawMessage) error {
		require.Eventually(t, func() bool {
			return fake.heartbeatCount() > 0
		}, time.Second, time.Millisecond)
		return nil
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          14,
		Kind:        "slow",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	heartbeats := fake.lastHeartbeats()
	require.NotEmpty(t, heartbeats, "a blocked handler must renew its lease")
	for _, hb := range heartbeats {
		assert.Equal(t, int64(14), hb.id)
		assert.Equal(t, "heartbeat-worker", hb.workerID)
	}

	before := fake.heartbeatCount()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, before, fake.heartbeatCount(), "heartbeat must stop before processJob returns")
	assert.Equal(t, []int64{14}, fake.succeededIDs())
	assert.Empty(t, fake.failures)
}

func TestProcessJobHeartbeatFailureDoesNotFailJob(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{heartbeatErr: errors.New("heartbeat unavailable")}
	worker := NewWorker(nil, "heartbeat-worker",
		WithHeartbeatInterval(5*time.Millisecond),
		WithLeaseDuration(250*time.Millisecond),
	)
	worker.store = fake
	worker.Register("slow", func(context.Context, json.RawMessage) error {
		require.Eventually(t, func() bool {
			return fake.heartbeatCount() > 0
		}, time.Second, time.Millisecond)
		return nil
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          15,
		Kind:        "slow",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	assert.GreaterOrEqual(t, fake.heartbeatCount(), 1)
	assert.Equal(t, []int64{15}, fake.succeededIDs(), "heartbeat errors must not change the job outcome")
	assert.Empty(t, fake.failures)
}

func TestPGStoreHeartbeatRequiresPool(t *testing.T) {
	t.Parallel()

	store := newPGStore(nil)
	err := store.Heartbeat(context.Background(), 1, "worker")
	require.ErrorIs(t, err, ErrNoPool)

	_, err = store.Claim(context.Background(), "worker", 1, time.Second)
	require.ErrorIs(t, err, ErrNoPool)
}

func TestQueueValidationBeforeDatabase(t *testing.T) {
	t.Parallel()

	queue := NewQueue(nil)

	_, err := queue.Enqueue(context.Background(), Job{})
	require.ErrorIs(t, err, ErrEmptyKind)

	_, err = queue.Enqueue(context.Background(), Job{Kind: "ok", Payload: json.RawMessage(`not-json`)})
	require.ErrorIs(t, err, ErrInvalidPayload)

	_, err = queue.Enqueue(context.Background(), Job{Kind: "ok"})
	require.ErrorIs(t, err, ErrNoPool)

	_, err = queue.EnqueueTx(context.Background(), nil, Job{Kind: "ok"})
	require.ErrorIs(t, err, ErrNoTx)
}

func TestQueuedSenderSendTxRequiresTransaction(t *testing.T) {
	t.Parallel()

	sender := NewQueuedSender(NewQueue(nil))
	var err error
	require.NotPanics(t, func() {
		err = sender.SendTx(context.Background(), nil, email.Message{
			To:      []string{"user@example.com"},
			Subject: "Verify Your Email Address",
		})
	})
	require.ErrorIs(t, err, ErrNoTx)

	// A nil sender or queue must fail cleanly instead of panicking.
	require.NotPanics(t, func() {
		err = (*QueuedSender)(nil).SendTx(context.Background(), nil, email.Message{})
	})
	require.Error(t, err)
}

// streamingStore serves jobs in claim-sized batches so a test can process more
// work than the worker's concurrency limit.
type streamingStore struct {
	*fakeStore
	pending []claimedJob
}

func (s *streamingStore) Claim(_ context.Context, _ string, limit int, staleAfter time.Duration) ([]claimedJob, error) {
	s.mu.Lock()
	s.claimCalls++
	s.lastStaleAfter = staleAfter
	s.mu.Unlock()

	if limit <= 0 || len(s.pending) == 0 {
		return nil, nil
	}
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	batch := append([]claimedJob(nil), s.pending[:limit]...)
	s.pending = s.pending[limit:]
	return batch, nil
}

// fakeStore records worker outcomes without a database.
type fakeStore struct {
	mu             sync.Mutex
	claimed        []claimedJob
	claimErr       error
	claimCalls     int
	lastStaleAfter time.Duration
	succeeded      []int64
	failures       []fakeFailure
	heartbeats     []fakeHeartbeat
	heartbeatErr   error
}

type fakeFailure struct {
	id       int64
	workerID string
	reason   string
	retryAt  *time.Time
}

type fakeHeartbeat struct {
	id       int64
	workerID string
}

func (f *fakeStore) Claim(_ context.Context, _ string, _ int, staleAfter time.Duration) ([]claimedJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.claimCalls++
	f.lastStaleAfter = staleAfter
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	jobs := f.claimed
	f.claimed = nil
	return jobs, nil
}

func (f *fakeStore) Succeed(_ context.Context, id int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeeded = append(f.succeeded, id)
	return nil
}

func (f *fakeStore) Fail(_ context.Context, id int64, workerID, reason string, retryAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fakeFailure{
		id:       id,
		workerID: workerID,
		reason:   reason,
		retryAt:  retryAt,
	})
	return nil
}

func (f *fakeStore) Heartbeat(_ context.Context, id int64, workerID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, fakeHeartbeat{id: id, workerID: workerID})
	return f.heartbeatErr
}

func (f *fakeStore) succeededIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.succeeded...)
}

func (f *fakeStore) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.heartbeats)
}

func (f *fakeStore) lastHeartbeats() []fakeHeartbeat {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeHeartbeat(nil), f.heartbeats...)
}

func (f *fakeStore) staleAfterSeen() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastStaleAfter
}
