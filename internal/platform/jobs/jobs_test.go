package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
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
	assert.Equal(t, 2, failure.attempts, "the claim attempt must be passed as the fence")
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
	assert.Equal(t, 3, fake.failures[0].attempts, "the claim attempt must be passed as the fence")
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

func TestProcessJobUnknownKindRetriesUntilExhausted(t *testing.T) {
	t.Parallel()

	t.Run("retries below the attempt limit", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "test-worker")
		worker.store = fake
		worker.retryBase = 10 * time.Millisecond
		worker.retryMax = time.Second

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID:          11,
			Kind:        "missing",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		require.Len(t, fake.failures, 1)
		assert.Contains(t, fake.failures[0].reason, ErrNoHandler.Error())
		require.NotNil(t, fake.failures[0].retryAt,
			"an unregistered kind must retry while attempts remain")
	})

	t.Run("dead-letters when exhausted", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "test-worker")
		worker.store = fake

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID:          11,
			Kind:        "missing",
			Payload:     json.RawMessage(`{}`),
			Attempts:    3,
			MaxAttempts: 3,
		}))

		require.Len(t, fake.failures, 1)
		assert.Contains(t, fake.failures[0].reason, ErrNoHandler.Error())
		assert.Nil(t, fake.failures[0].retryAt,
			"an unregistered kind must be dead-lettered only after max attempts")
	})
}

func TestProcessJobUsesAttemptFence(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "fence-worker")
		worker.store = fake
		worker.Register("ok", func(context.Context, json.RawMessage) error { return nil })

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID:          30,
			Kind:        "ok",
			Payload:     json.RawMessage(`{}`),
			Attempts:    3,
			MaxAttempts: 5,
		}))

		require.Len(t, fake.succeeded, 1)
		assert.Equal(t, 3, fake.succeeded[0].attempts)
	})

	t.Run("failure", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "fence-worker")
		worker.store = fake
		worker.Register("boom", func(context.Context, json.RawMessage) error {
			return errors.New("boom")
		})

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID:          31,
			Kind:        "boom",
			Payload:     json.RawMessage(`{}`),
			Attempts:    3,
			MaxAttempts: 5,
		}))

		require.Len(t, fake.failures, 1)
		assert.Equal(t, 3, fake.failures[0].attempts)
	})
}

func TestWorkerJobTimeoutCancelsHandler(t *testing.T) {
	t.Parallel()

	t.Run("enabled timeout cancels a blocking handler", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "timeout-worker", WithJobTimeout(20*time.Millisecond))
		worker.store = fake
		worker.Register("blocking", func(ctx context.Context, _ json.RawMessage) error {
			<-ctx.Done()
			return ctx.Err()
		})

		start := time.Now()
		require.NoError(t, processJobWithWatchdog(t, worker, context.Background(), claimedJob{
			ID:          32,
			Kind:        "blocking",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))
		assert.Less(t, time.Since(start), time.Second, "the handler must be canceled by the job timeout")

		require.Len(t, fake.failures, 1)
		assert.Contains(t, fake.failures[0].reason, "deadline exceeded")
		assert.NotNil(t, fake.failures[0].retryAt, "a timed-out job with attempts left must retry")
		assert.Empty(t, fake.succeededIDs())
	})

	t.Run("enabled timeout classifies a late nil return", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "timeout-worker", WithJobTimeout(20*time.Millisecond))
		worker.store = fake
		worker.Register("ignores-context", func(context.Context, json.RawMessage) error {
			time.Sleep(50 * time.Millisecond)
			return nil
		})

		require.NoError(t, processJobWithWatchdog(t, worker, context.Background(), claimedJob{
			ID:          33,
			Kind:        "ignores-context",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		require.Len(t, fake.failures, 1)
		assert.Contains(t, fake.failures[0].reason, "job timeout")
		assert.Empty(t, fake.succeededIDs(),
			"a handler that returns nil after its deadline must not be marked succeeded")
	})

	t.Run("disabled timeout does not misclassify parent cancellation", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "timeout-worker", WithJobTimeout(0))
		worker.store = fake
		worker.Register("blocking", func(ctx context.Context, _ json.RawMessage) error {
			<-ctx.Done()
			return nil
		})

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		require.NoError(t, processJobWithWatchdog(t, worker, ctx, claimedJob{
			ID:          34,
			Kind:        "blocking",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		assert.Equal(t, []int64{34}, fake.succeededIDs(),
			"a disabled job timeout must not turn a successful handler into a timeout failure")
		assert.Empty(t, fake.failures,
			"a disabled timeout must not produce a handler exceeded the 0s job timeout failure")
	})

	t.Run("expired parent deadline is not classified as job timeout", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "timeout-worker", WithJobTimeout(time.Minute))
		worker.store = fake
		worker.Register("ok", func(context.Context, json.RawMessage) error {
			return nil
		})

		parent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		require.ErrorIs(t, parent.Err(), context.DeadlineExceeded)

		require.NoError(t, processJobWithWatchdog(t, worker, parent, claimedJob{
			ID:          35,
			Kind:        "ok",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		assert.Equal(t, []int64{35}, fake.succeededIDs(),
			"an already-expired parent deadline must not be mislabeled as a job timeout")
		for _, failure := range fake.failures {
			assert.NotContains(t, failure.reason, "job timeout")
		}
		assert.Empty(t, fake.failures,
			"the handler's nil result must be recorded as success despite the expired parent")
	})
}

// processJobWithWatchdog runs processJob in its own goroutine and fails the
// test instead of hanging forever when a regression stops it from returning.
func processJobWithWatchdog(t *testing.T, worker *Worker, ctx context.Context, job claimedJob) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- worker.processJob(ctx, job) }()

	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("processJob did not return within 2s")
		return nil
	}
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

func TestWorkerRunRetriesClaimError(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{
		claimErrs: []error{errors.New("database unavailable")},
		claimed: []claimedJob{{
			ID:          16,
			Kind:        "ok",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}},
	}
	worker := NewWorker(nil, "test-worker", WithPollInterval(time.Millisecond))
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
		t.Fatal("worker did not retry the claim and process the job")
	}

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err, "a transient claim error must not terminate the worker")
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	assert.GreaterOrEqual(t, fake.claimCalls, 2, "the failed claim must be retried")
	assert.Equal(t, []int64{16}, fake.succeededIDs())
}

func TestWorkerRunPassesAttemptFence(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{claimed: []claimedJob{{
		ID:          17,
		Kind:        "slow",
		Payload:     json.RawMessage(`{}`),
		Attempts:    3,
		MaxAttempts: 5,
	}}}
	worker := NewWorker(nil, "fence-worker",
		WithPollInterval(time.Millisecond),
		WithHeartbeatInterval(5*time.Millisecond),
		WithLeaseDuration(250*time.Millisecond),
	)
	worker.store = fake

	handled := make(chan struct{})
	worker.Register("slow", func(ctx context.Context, _ json.RawMessage) error {
		deadline := time.Now().Add(time.Second)
		for fake.heartbeatCount() == 0 {
			if time.Now().After(deadline) {
				return errors.New("heartbeat was not sent")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
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

	require.Len(t, fake.succeeded, 1)
	assert.Equal(t, 3, fake.succeeded[0].attempts)
	heartbeats := fake.lastHeartbeats()
	require.NotEmpty(t, heartbeats)
	for _, hb := range heartbeats {
		assert.Equal(t, 3, hb.attempts, "heartbeats must carry the claim attempt fence")
	}
}

func TestWorkerOptions(t *testing.T) {
	t.Parallel()

	worker := NewWorker(nil, "",
		WithPollInterval(0),
		WithConcurrency(0),
		WithHeartbeatInterval(0),
		WithLeaseDuration(-time.Second),
		WithJobTimeout(0),
	)
	assert.Equal(t, defaultPollInterval, worker.pollInterval)
	assert.Equal(t, defaultConcurrency, worker.concurrency)
	assert.Equal(t, defaultHeartbeatInterval, worker.heartbeatInterval)
	assert.Equal(t, defaultLeaseDuration, worker.leaseDuration)
	assert.Equal(t, time.Duration(0), worker.jobTimeout, "a non-positive timeout disables the deadline")
	assert.Equal(t, "worker", worker.id)

	worker = NewWorker(nil, "w",
		WithPollInterval(25*time.Millisecond),
		WithConcurrency(4),
		WithHeartbeatInterval(2*time.Second),
		WithLeaseDuration(20*time.Second),
		WithJobTimeout(2*time.Minute),
	)
	assert.Equal(t, 25*time.Millisecond, worker.pollInterval)
	assert.Equal(t, 4, worker.concurrency)
	assert.Equal(t, 2*time.Second, worker.heartbeatInterval)
	assert.Equal(t, 20*time.Second, worker.leaseDuration)
	assert.Equal(t, 2*time.Minute, worker.jobTimeout)

	assert.Equal(t, defaultJobTimeout, NewWorker(nil, "w").jobTimeout)
	assert.Equal(t, defaultRetention, NewWorker(nil, "w").retention)
	assert.Equal(t, defaultPurgeInterval, NewWorker(nil, "w").purgeInterval)

	// Retention is enabled by default, disabled by a non-positive window, and
	// the purge interval ignores non-positive values.
	worker = NewWorker(nil, "w", WithRetention(0), WithPurgeInterval(0))
	assert.Equal(t, time.Duration(0), worker.retention)
	assert.Equal(t, defaultPurgeInterval, worker.purgeInterval)

	worker = NewWorker(nil, "w", WithRetention(48*time.Hour), WithPurgeInterval(5*time.Minute))
	assert.Equal(t, 48*time.Hour, worker.retention)
	assert.Equal(t, 5*time.Minute, worker.purgeInterval)

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

// metricValue reads a Prometheus counter's current value.
func metricValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	return testutil.ToFloat64(counter)
}

// TestWorkerMetrics checks that the job lifecycle counters move exactly once
// per recorded outcome and only after the database write succeeds. It is not
// parallel because the claim-error and purge counters are not labelled.
func TestWorkerMetrics(t *testing.T) {
	t.Run("claim error, claim, and success", func(t *testing.T) {
		const kind = "metrics.claim.success"
		fake := &fakeStore{
			claimErrs: []error{errors.New("database unavailable")},
			claimed: []claimedJob{{
				ID:          100,
				Kind:        kind,
				Payload:     json.RawMessage(`{}`),
				Attempts:    1,
				MaxAttempts: 3,
			}},
		}
		worker := NewWorker(nil, "metrics-worker", WithPollInterval(time.Millisecond))
		worker.store = fake

		handled := make(chan struct{})
		worker.Register(kind, func(context.Context, json.RawMessage) error {
			close(handled)
			return nil
		})

		claimedBefore := metricValue(t, metrics.JobsClaimedTotal.WithLabelValues(kind))
		succeededBefore := metricValue(t, metrics.JobsSucceededTotal.WithLabelValues(kind))
		claimErrorsBefore := metricValue(t, metrics.JobsClaimErrorsTotal)

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- worker.Run(ctx) }()

		select {
		case <-handled:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("worker did not process the job")
		}
		cancel()
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not stop after context cancellation")
		}

		assert.Equal(t, claimedBefore+1,
			metricValue(t, metrics.JobsClaimedTotal.WithLabelValues(kind)))
		assert.Equal(t, succeededBefore+1,
			metricValue(t, metrics.JobsSucceededTotal.WithLabelValues(kind)))
		assert.Equal(t, claimErrorsBefore+1, metricValue(t, metrics.JobsClaimErrorsTotal))
	})

	t.Run("retried", func(t *testing.T) {
		const kind = "metrics.retried"
		fake := &fakeStore{}
		worker := NewWorker(nil, "metrics-worker")
		worker.store = fake
		worker.retryBase = time.Millisecond
		worker.retryMax = time.Millisecond
		worker.Register(kind, func(context.Context, json.RawMessage) error {
			return errors.New("temporary failure")
		})

		retriedBefore := metricValue(t, metrics.JobsRetriedTotal.WithLabelValues(kind))
		deadBefore := metricValue(t, metrics.JobsDeadTotal.WithLabelValues(kind))

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID:          101,
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		assert.Equal(t, retriedBefore+1,
			metricValue(t, metrics.JobsRetriedTotal.WithLabelValues(kind)))
		assert.Equal(t, deadBefore,
			metricValue(t, metrics.JobsDeadTotal.WithLabelValues(kind)))
	})

	t.Run("dead", func(t *testing.T) {
		const kind = "metrics.dead"
		fake := &fakeStore{}
		worker := NewWorker(nil, "metrics-worker")
		worker.store = fake
		worker.Register(kind, func(context.Context, json.RawMessage) error {
			return errors.New("permanent failure")
		})

		retriedBefore := metricValue(t, metrics.JobsRetriedTotal.WithLabelValues(kind))
		deadBefore := metricValue(t, metrics.JobsDeadTotal.WithLabelValues(kind))

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID:          102,
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    3,
			MaxAttempts: 3,
		}))

		assert.Equal(t, retriedBefore,
			metricValue(t, metrics.JobsRetriedTotal.WithLabelValues(kind)))
		assert.Equal(t, deadBefore+1,
			metricValue(t, metrics.JobsDeadTotal.WithLabelValues(kind)))
	})

	t.Run("bookkeeping failure is not counted", func(t *testing.T) {
		const kind = "metrics.bookkeeping.failure"
		fake := &fakeStore{succeedErr: errors.New("database unavailable")}
		worker := NewWorker(nil, "metrics-worker")
		worker.store = fake
		worker.Register(kind, func(context.Context, json.RawMessage) error { return nil })

		succeededBefore := metricValue(t, metrics.JobsSucceededTotal.WithLabelValues(kind))

		err := worker.processJob(context.Background(), claimedJob{
			ID:          103,
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		})
		require.Error(t, err)

		assert.Equal(t, succeededBefore,
			metricValue(t, metrics.JobsSucceededTotal.WithLabelValues(kind)),
			"a failed success write must not count as a succeeded outcome")
	})

	t.Run("failed fail write is not counted", func(t *testing.T) {
		const kind = "metrics.bookkeeping.fail"
		fake := &fakeStore{failErr: errors.New("database unavailable")}
		worker := NewWorker(nil, "metrics-worker")
		worker.store = fake
		worker.Register(kind, func(context.Context, json.RawMessage) error {
			return errors.New("handler failure")
		})

		retriedBefore := metricValue(t, metrics.JobsRetriedTotal.WithLabelValues(kind))
		deadBefore := metricValue(t, metrics.JobsDeadTotal.WithLabelValues(kind))

		err := worker.processJob(context.Background(), claimedJob{
			ID:          104,
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		})
		require.Error(t, err)

		assert.Equal(t, retriedBefore,
			metricValue(t, metrics.JobsRetriedTotal.WithLabelValues(kind)))
		assert.Equal(t, deadBefore,
			metricValue(t, metrics.JobsDeadTotal.WithLabelValues(kind)))
	})
}

func TestWorkerRunPurgesTerminalJobsOnInterval(t *testing.T) {
	fake := &fakeStore{purgeRows: 3}
	worker := NewWorker(nil, "purge-worker",
		WithPollInterval(time.Millisecond),
		WithRetention(time.Hour),
		WithPurgeInterval(10*time.Millisecond),
	)
	worker.store = fake

	before := metricValue(t, metrics.JobsPurgedTotal)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	calls := fake.purgeCallCount()
	require.GreaterOrEqual(t, calls, 1, "the worker must run retention on the configured interval")
	assert.Equal(t, time.Hour, fake.retentionSeen())
	assert.Equal(t, before+float64(calls)*3, metricValue(t, metrics.JobsPurgedTotal))
}

func TestWorkerRunPurgeFailureIsLoggedAndRetried(t *testing.T) {
	fake := &fakeStore{purgeErr: errors.New("purge unavailable")}
	worker := NewWorker(nil, "purge-worker",
		WithPollInterval(time.Millisecond),
		WithRetention(time.Hour),
		WithPurgeInterval(10*time.Millisecond),
	)
	worker.store = fake

	before := metricValue(t, metrics.JobsPurgedTotal)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	require.Eventually(t, func() bool {
		return fake.purgeCallCount() >= 2
	}, 2*time.Second, 5*time.Millisecond, "a failed purge must not stop later attempts")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	assert.Equal(t, before, metricValue(t, metrics.JobsPurgedTotal),
		"a failed purge must not count as purged rows")
}

func TestWorkerRunRetentionDisabled(t *testing.T) {
	fake := &fakeStore{purgeRows: 1}
	worker := NewWorker(nil, "purge-worker",
		WithPollInterval(time.Millisecond),
		WithRetention(0),
		WithPurgeInterval(time.Millisecond),
	)
	worker.store = fake

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	assert.Equal(t, 0, fake.purgeCallCount(), "a non-positive retention must disable purging")
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
		assert.Equal(t, 1, hb.attempts)
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
	err := store.Heartbeat(context.Background(), 1, "worker", 1)
	require.ErrorIs(t, err, ErrNoPool)

	_, err = store.Claim(context.Background(), "worker", 1, time.Second)
	require.ErrorIs(t, err, ErrNoPool)
}

func TestPGStorePurgeTerminalValidation(t *testing.T) {
	t.Parallel()

	store := newPGStore(nil)

	_, err := store.PurgeTerminal(context.Background(), 0)
	require.ErrorIs(t, err, ErrInvalidRetention)

	_, err = store.PurgeTerminal(context.Background(), -time.Hour)
	require.ErrorIs(t, err, ErrInvalidRetention)

	_, err = store.PurgeTerminal(context.Background(), time.Hour)
	require.ErrorIs(t, err, ErrNoPool)

	var nilStore *pgStore
	_, err = nilStore.PurgeTerminal(context.Background(), time.Hour)
	require.ErrorIs(t, err, ErrNoPool)
}

func TestQueuePurgeTerminalValidation(t *testing.T) {
	t.Parallel()

	_, err := NewQueue(nil).PurgeTerminal(context.Background(), 30*24*time.Hour)
	require.ErrorIs(t, err, ErrNoPool)

	_, err = NewQueue(nil).PurgeTerminal(context.Background(), 0)
	require.ErrorIs(t, err, ErrInvalidRetention)

	_, err = NewQueue(nil).PurgeTerminal(context.Background(), -time.Second)
	require.ErrorIs(t, err, ErrInvalidRetention)

	var nilQueue *Queue
	_, err = nilQueue.PurgeTerminal(context.Background(), time.Hour)
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

func TestQueuedSenderSendRequiresPool(t *testing.T) {
	t.Parallel()

	sender := NewQueuedSender(NewQueue(nil))
	var err error
	require.NotPanics(t, func() {
		err = sender.Send(context.Background(), email.Message{
			To:      []string{"user@example.com"},
			Subject: "Verify Your Email Address",
		})
	})
	require.ErrorIs(t, err, ErrNoPool)

	// A nil sender or queue must fail cleanly instead of panicking.
	require.NotPanics(t, func() {
		err = (*QueuedSender)(nil).Send(context.Background(), email.Message{})
	})
	require.Error(t, err)
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
	claimErrs      []error
	claimCalls     int
	lastStaleAfter time.Duration
	succeeded      []fakeSuccess
	succeedErr     error
	failures       []fakeFailure
	failErr        error
	heartbeats     []fakeHeartbeat
	heartbeatErr   error
	purgeCalls     int
	purgeRows      int64
	purgeErr       error
	lastRetention  time.Duration
}

type fakeSuccess struct {
	id       int64
	workerID string
	attempts int
}

type fakeFailure struct {
	id       int64
	workerID string
	attempts int
	reason   string
	retryAt  *time.Time
}

type fakeHeartbeat struct {
	id       int64
	workerID string
	attempts int
}

func (f *fakeStore) Claim(_ context.Context, _ string, _ int, staleAfter time.Duration) ([]claimedJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.claimCalls++
	f.lastStaleAfter = staleAfter
	if len(f.claimErrs) > 0 {
		err := f.claimErrs[0]
		f.claimErrs = f.claimErrs[1:]
		return nil, err
	}
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	jobs := f.claimed
	f.claimed = nil
	return jobs, nil
}

func (f *fakeStore) Succeed(_ context.Context, id int64, workerID string, attempts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeeded = append(f.succeeded, fakeSuccess{id: id, workerID: workerID, attempts: attempts})
	return f.succeedErr
}

func (f *fakeStore) Fail(_ context.Context, id int64, workerID string, attempts int, reason string, retryAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, fakeFailure{
		id:       id,
		workerID: workerID,
		attempts: attempts,
		reason:   reason,
		retryAt:  retryAt,
	})
	return f.failErr
}

func (f *fakeStore) Heartbeat(_ context.Context, id int64, workerID string, attempts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, fakeHeartbeat{id: id, workerID: workerID, attempts: attempts})
	return f.heartbeatErr
}

func (f *fakeStore) PurgeTerminal(_ context.Context, olderThan time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purgeCalls++
	f.lastRetention = olderThan
	return f.purgeRows, f.purgeErr
}

func (f *fakeStore) purgeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.purgeCalls
}

func (f *fakeStore) retentionSeen() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRetention
}

func (f *fakeStore) succeededIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int64, len(f.succeeded))
	for i, success := range f.succeeded {
		ids[i] = success.id
	}
	return ids
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
