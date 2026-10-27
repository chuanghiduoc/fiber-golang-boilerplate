package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

func TestJobNormalize(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	t.Run("applies defaults", func(t *testing.T) {
		t.Parallel()

		job, err := Job{Kind: "email.send.encrypted"}.normalize(now)
		require.NoError(t, err)
		assert.Equal(t, "email.send.encrypted", job.Kind)
		assert.JSONEq(t, `{}`, string(job.Payload))
		assert.Equal(t, now, job.RunAt)
		assert.Equal(t, DefaultMaxAttempts, job.MaxAttempts)
	})

	t.Run("preserves explicit values", func(t *testing.T) {
		t.Parallel()

		runAt := now.Add(time.Hour)
		job, err := Job{
			Kind:        "email.send.encrypted",
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

		_, err := Job{Kind: "email.send.encrypted", Payload: json.RawMessage(`{`)}.normalize(now)
		require.ErrorIs(t, err, ErrInvalidPayload)
	})

	t.Run("rejects negative attempts", func(t *testing.T) {
		t.Parallel()

		_, err := Job{Kind: "email.send.encrypted", MaxAttempts: -1}.normalize(now)
		require.ErrorIs(t, err, ErrInvalidMaxAttempts)
	})

	t.Run("normalizes idempotency key", func(t *testing.T) {
		t.Parallel()

		job, err := Job{Kind: "email.send.encrypted", IdempotencyKey: "  event:1  "}.normalize(now)
		require.NoError(t, err)
		assert.Equal(t, "event:1", job.IdempotencyKey)

		_, err = Job{Kind: "email.send.encrypted", IdempotencyKey: strings.Repeat("x", 513)}.normalize(now)
		require.ErrorIs(t, err, ErrInvalidIdempotencyKey)
	})
}

func TestPrepareJobTenant(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		ctx     context.Context
		job     Job
		want    uuid.UUID
		wantErr error
	}{
		{
			name:    "empty context has no tenant",
			ctx:     context.Background(),
			job:     Job{Kind: "email.send.encrypted"},
			wantErr: ErrInvalidTenant,
		},
		{
			name: "context tenant is inherited",
			ctx:  tenant.WithID(context.Background(), testID(2)),
			job:  Job{Kind: "email.send.encrypted"},
			want: testID(2),
		},
		{
			name: "explicit tenant is preserved",
			ctx:  tenant.WithID(context.Background(), testID(2)),
			job:  Job{Kind: "email.send.encrypted", TenantID: testID(3)},
			want: testID(3),
		},
		{
			name:    "non-positive tenant is rejected",
			ctx:     context.Background(),
			job:     Job{Kind: "email.send.encrypted", TenantID: uuid.Nil},
			wantErr: ErrInvalidTenant,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			job, err := prepareJob(tt.ctx, tt.job, now)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, job.TenantID)
		})
	}
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
		TenantID:    testID(2),
		ID:          testID(7),
		Kind:        "ok",
		Payload:     json.RawMessage(`{"value":1}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	assert.Equal(t, []uuid.UUID{testID(7)}, fake.succeededIDs())
	assert.Empty(t, fake.failures)
}

func TestProcessJobPropagatesTenantToHandler(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake

	var seen uuid.UUID
	worker.Register("tenant", func(ctx context.Context, _ json.RawMessage) error {
		seen = tenant.IDFromContext(ctx)
		return nil
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          testID(21),
		TenantID:    testID(2),
		Kind:        "tenant",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	assert.Equal(t, testID(2), seen, "handler must run under the job tenant")
	assert.Equal(t, []uuid.UUID{testID(21)}, fake.succeededIDs())
}

func TestProcessJobDeadLettersMissingTenant(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker")
	worker.store = fake
	ran := false
	worker.Register("tenant", func(context.Context, json.RawMessage) error {
		ran = true
		return nil
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          testID(22),
		Kind:        "tenant",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	assert.False(t, ran, "a job without a tenant must never run")
	assert.Empty(t, fake.succeededIDs())
	require.Len(t, fake.failures, 1)
	assert.Nil(t, fake.failures[0].retryAt, "a missing tenant is a permanent failure")
	assert.Contains(t, fake.failures[0].reason, "no tenant")
	assert.Contains(t, fake.failures[0].reason, "permanent failure (not retried)")
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
		TenantID:    testID(2),
		ID:          testID(8),
		Kind:        "boom",
		Payload:     json.RawMessage(`{}`),
		Attempts:    2,
		MaxAttempts: 5,
	}))

	require.Len(t, fake.failures, 1)
	failure := fake.failures[0]
	assert.Equal(t, testID(8), failure.id)
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
		TenantID:    testID(2),
		ID:          testID(9),
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
		TenantID:    testID(2),
		ID:          testID(10),
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
			TenantID:    testID(2),
			ID:          testID(11),
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
			TenantID:    testID(2),
			ID:          testID(11),
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
			TenantID:    testID(2),
			ID:          testID(30),
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
			TenantID:    testID(2),
			ID:          testID(31),
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
			TenantID:    testID(2),
			ID:          testID(32),
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

	t.Run("enabled timeout records a late nil return as success", func(t *testing.T) {
		t.Parallel()

		fake := &fakeStore{}
		worker := NewWorker(nil, "timeout-worker", WithJobTimeout(20*time.Millisecond))
		worker.store = fake
		worker.Register("ignores-context", func(context.Context, json.RawMessage) error {
			time.Sleep(50 * time.Millisecond)
			return nil
		})

		require.NoError(t, processJobWithWatchdog(t, worker, context.Background(), claimedJob{
			TenantID:    testID(2),
			ID:          testID(33),
			Kind:        "ignores-context",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		assert.Equal(t, []uuid.UUID{testID(33)}, fake.succeededIDs(),
			"a handler that returns nil after its deadline already ran its side effects; it must be marked succeeded")
		assert.Empty(t, fake.failures,
			"recording a failure would re-run a handler that reported success")
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
			TenantID:    testID(2),
			ID:          testID(34),
			Kind:        "blocking",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		assert.Equal(t, []uuid.UUID{testID(34)}, fake.succeededIDs(),
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
			TenantID:    testID(2),
			ID:          testID(35),
			Kind:        "ok",
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		assert.Equal(t, []uuid.UUID{testID(35)}, fake.succeededIDs(),
			"an already-expired parent deadline must not be mislabeled as a job timeout")
		for _, failure := range fake.failures {
			assert.NotContains(t, failure.reason, "job timeout")
		}
		assert.Empty(t, fake.failures,
			"the handler's nil result must be recorded as success despite the expired parent")
	})
}

// TestProcessJobShutdownReleasesWithoutConsumingAttempt proves a handler that
// fails because the worker is stopping is requeued, not dead-lettered, even on
// its final attempt.
func TestProcessJobShutdownReleasesWithoutConsumingAttempt(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{}
	worker := NewWorker(nil, "shutdown-worker")
	worker.store = fake
	worker.Register("blocking", func(ctx context.Context, _ json.RawMessage) error {
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	defer cancel()

	require.NoError(t, processJobWithWatchdog(t, worker, ctx, claimedJob{
		TenantID:    testID(2),
		ID:          testID(36),
		Kind:        "blocking",
		Payload:     json.RawMessage(`{}`),
		Attempts:    3,
		MaxAttempts: 3,
	}))

	assert.Empty(t, fake.failures, "a shutdown interruption must not dead-letter the job")
	assert.Empty(t, fake.succeededIDs())
	require.Len(t, fake.releases, 1)
	assert.Equal(t, testID(36), fake.releases[0].id)
	assert.Equal(t, 3, fake.releases[0].attempts, "the release is fenced by the claimed attempt")
	assert.Contains(t, fake.releases[0].reason, "interrupted by worker shutdown")
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

	fake := &fakeStore{claimed: []claimedJob{{TenantID: testID(2),
		ID:          testID(12),
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
	assert.Equal(t, []uuid.UUID{testID(12)}, fake.succeededIDs())
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
		claimed: []claimedJob{{TenantID: testID(2),
			ID:          testID(16),
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
	assert.Equal(t, []uuid.UUID{testID(16)}, fake.succeededIDs())
}

func TestWorkerRunPassesAttemptFence(t *testing.T) {
	t.Parallel()

	fake := &fakeStore{claimed: []claimedJob{{TenantID: testID(2),
		ID:          testID(17),
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

// TestMain installs the metric provider so the job instruments behind the
// package-level wrappers are live for the whole package run.
func TestMain(m *testing.M) {
	provider, err := metrics.Setup(metrics.Options{})
	if err != nil {
		panic(err)
	}
	pgtest.Start()
	code := m.Run()
	pgtest.Stop()
	_ = provider.Shutdown(context.Background())
	os.Exit(code)
}

// metricValue reads a current counter or gauge value from the Prometheus
// registry without pulling in prometheus/testutil. labels are key/value pairs.
func metricValue(t *testing.T, name string, labels ...string) float64 {
	t.Helper()
	if len(labels)%2 != 0 {
		t.Fatalf("metricValue(%q): labels must be key/value pairs", name)
	}
	families, err := metrics.Registry().Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			if !metricLabelsMatch(m, labels) {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func metricLabelsMatch(metric *dto.Metric, labels []string) bool {
	want := make(map[string]string, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	for _, pair := range metric.GetLabel() {
		if value, ok := want[pair.GetName()]; ok && value == pair.GetValue() {
			delete(want, pair.GetName())
		}
	}
	return len(want) == 0
}

// TestWorkerMetrics checks that the job lifecycle counters move exactly once
// per recorded outcome and only after the database write succeeds. It is not
// parallel because the claim-error and purge counters are not labelled.
func TestWorkerMetrics(t *testing.T) {
	t.Run("claim error, claim, and success", func(t *testing.T) {
		const kind = "metrics.claim.success"
		fake := &fakeStore{
			claimErrs: []error{errors.New("database unavailable")},
			claimed: []claimedJob{{TenantID: testID(2),
				ID:          testID(100),
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

		claimedBefore := metricValue(t, "jobs_claimed_total", "kind", kind)
		succeededBefore := metricValue(t, "jobs_succeeded_total", "kind", kind)
		claimErrorsBefore := metricValue(t, "jobs_claim_errors_total")

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
			metricValue(t, "jobs_claimed_total", "kind", kind))
		assert.Equal(t, succeededBefore+1,
			metricValue(t, "jobs_succeeded_total", "kind", kind))
		assert.Equal(t, claimErrorsBefore+1, metricValue(t, "jobs_claim_errors_total"))
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

		retriedBefore := metricValue(t, "jobs_retried_total", "kind", kind)
		deadBefore := metricValue(t, "jobs_dead_total", "kind", kind)

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			TenantID:    testID(2),
			ID:          testID(101),
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		}))

		assert.Equal(t, retriedBefore+1,
			metricValue(t, "jobs_retried_total", "kind", kind))
		assert.Equal(t, deadBefore,
			metricValue(t, "jobs_dead_total", "kind", kind))
	})

	t.Run("dead", func(t *testing.T) {
		const kind = "metrics.dead"
		fake := &fakeStore{}
		worker := NewWorker(nil, "metrics-worker")
		worker.store = fake
		worker.Register(kind, func(context.Context, json.RawMessage) error {
			return errors.New("permanent failure")
		})

		retriedBefore := metricValue(t, "jobs_retried_total", "kind", kind)
		deadBefore := metricValue(t, "jobs_dead_total", "kind", kind)

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			TenantID:    testID(2),
			ID:          testID(102),
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    3,
			MaxAttempts: 3,
		}))

		assert.Equal(t, retriedBefore,
			metricValue(t, "jobs_retried_total", "kind", kind))
		assert.Equal(t, deadBefore+1,
			metricValue(t, "jobs_dead_total", "kind", kind))
	})

	t.Run("bookkeeping failure is not counted", func(t *testing.T) {
		const kind = "metrics.bookkeeping.failure"
		fake := &fakeStore{succeedErr: errors.New("database unavailable")}
		worker := NewWorker(nil, "metrics-worker")
		worker.store = fake
		worker.Register(kind, func(context.Context, json.RawMessage) error { return nil })

		succeededBefore := metricValue(t, "jobs_succeeded_total", "kind", kind)

		err := worker.processJob(context.Background(), claimedJob{
			TenantID:    testID(2),
			ID:          testID(103),
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		})
		require.Error(t, err)

		assert.Equal(t, succeededBefore,
			metricValue(t, "jobs_succeeded_total", "kind", kind),
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

		retriedBefore := metricValue(t, "jobs_retried_total", "kind", kind)
		deadBefore := metricValue(t, "jobs_dead_total", "kind", kind)

		err := worker.processJob(context.Background(), claimedJob{
			TenantID:    testID(2),
			ID:          testID(104),
			Kind:        kind,
			Payload:     json.RawMessage(`{}`),
			Attempts:    1,
			MaxAttempts: 3,
		})
		require.Error(t, err)

		assert.Equal(t, retriedBefore,
			metricValue(t, "jobs_retried_total", "kind", kind))
		assert.Equal(t, deadBefore,
			metricValue(t, "jobs_dead_total", "kind", kind))
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

	before := metricValue(t, "jobs_purged_total")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	require.Eventually(t, func() bool {
		// One immediate startup pass plus at least one interval pass proves
		// recurrence, not just the startup purge.
		return fake.purgeCallCount() >= 2
	}, 2*time.Second, time.Millisecond, "the worker must run retention on the configured interval")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	calls := fake.purgeCallCount()
	require.GreaterOrEqual(t, calls, 2)
	assert.Equal(t, time.Hour, fake.retentionSeen())
	assert.Equal(t, before+float64(calls)*3, metricValue(t, "jobs_purged_total"))
}

func TestWorkerRunPurgesImmediatelyOnStartup(t *testing.T) {
	fake := &fakeStore{purgeRows: 2}
	worker := NewWorker(nil, "purge-worker",
		WithPollInterval(time.Hour),
		WithRetention(30*24*time.Hour),
		WithPurgeInterval(time.Hour),
	)
	worker.store = fake

	before := metricValue(t, "jobs_purged_total")

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	require.Eventually(t, func() bool {
		return fake.purgeCallCount() >= 1
	}, 2*time.Second, time.Millisecond,
		"a fresh worker must run retention without waiting a full interval")

	cancel()
	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}

	calls := fake.purgeCallCount()
	require.GreaterOrEqual(t, calls, 1)
	assert.Equal(t, 30*24*time.Hour, fake.retentionSeen())
	assert.Equal(t, before+float64(calls)*2, metricValue(t, "jobs_purged_total"))
}

func TestWorkerRunPurgeFailureIsLoggedAndRetried(t *testing.T) {
	fake := &fakeStore{
		purgeRows: 2,
		purgeErr:  errors.New("purge unavailable"),
	}
	worker := NewWorker(nil, "purge-worker",
		WithPollInterval(time.Millisecond),
		WithRetention(time.Hour),
		WithPurgeInterval(10*time.Millisecond),
	)
	worker.store = fake

	before := metricValue(t, "jobs_purged_total")

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

	calls := fake.purgeCallCount()
	assert.Equal(t, before+float64(calls)*2, metricValue(t, "jobs_purged_total"),
		"rows deleted before a batch failure must still be counted")
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

	require.Eventually(t, func() bool {
		return fake.claimCallCount() >= 1
	}, 2*time.Second, time.Millisecond, "worker must enter its claim loop")

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

	fake := &fakeStore{claimed: []claimedJob{{TenantID: testID(2),
		ID:          testID(13),
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
			TenantID:    testID(2),
			ID:          testID(i + 1),
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
		TenantID:    testID(2),
		ID:          testID(14),
		Kind:        "slow",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	heartbeats := fake.lastHeartbeats()
	require.NotEmpty(t, heartbeats, "a blocked handler must renew its lease")
	for _, hb := range heartbeats {
		assert.Equal(t, testID(14), hb.id)
		assert.Equal(t, "heartbeat-worker", hb.workerID)
		assert.Equal(t, 1, hb.attempts)
	}

	before := fake.heartbeatCount()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, before, fake.heartbeatCount(), "heartbeat must stop before processJob returns")
	assert.Equal(t, []uuid.UUID{testID(14)}, fake.succeededIDs())
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
	// Freeze the worker clock: the failure happens well inside the cancel
	// margin, so it must stay a transient error.
	worker.now = func() time.Time { return time.Unix(0, 0) }
	worker.Register("slow", func(context.Context, json.RawMessage) error {
		require.Eventually(t, func() bool {
			return fake.heartbeatCount() > 0
		}, time.Second, time.Millisecond)
		return nil
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		TenantID:    testID(2),
		ID:          testID(15),
		Kind:        "slow",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	assert.GreaterOrEqual(t, fake.heartbeatCount(), 1)
	assert.Equal(t, []uuid.UUID{testID(15)}, fake.succeededIDs(), "a transient heartbeat error before lease expiry must not change the outcome")
	assert.Empty(t, fake.failures)
}

// TestProcessJobLeaseLostCancelsHandler proves the heartbeat stops a handler as
// soon as the fenced renewal reports that another worker owns the job, and
// that this worker records no outcome for it.
func TestProcessJobLeaseLostCancelsHandler(t *testing.T) {
	t.Parallel()

	const kind = "lease.lost"
	fake := &fakeStore{heartbeatErr: fmt.Errorf("%w: job was reclaimed", ErrLeaseLost)}
	worker := NewWorker(nil, "lease-worker",
		WithHeartbeatInterval(5*time.Millisecond),
		WithLeaseDuration(250*time.Millisecond),
	)
	worker.store = fake

	before := metricValue(t, "jobs_lease_lost_total", "kind", kind)
	cause := make(chan error, 1)
	worker.Register(kind, func(ctx context.Context, _ json.RawMessage) error {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return ctx.Err()
	})

	require.NoError(t, processJobWithWatchdog(t, worker, context.Background(), claimedJob{
		TenantID:    testID(2),
		ID:          testID(21),
		Kind:        kind,
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	select {
	case got := <-cause:
		assert.ErrorIs(t, got, ErrLeaseLost, "handler must see the lease-loss cause")
	case <-time.After(time.Second):
		t.Fatal("handler was not canceled after losing the lease")
	}
	assert.Empty(t, fake.succeededIDs(), "a lost lease must not record success")
	assert.Empty(t, fake.failures, "a lost lease must not record a handler failure")
	assert.Empty(t, fake.releases, "a lost lease must not release the new owner's job")
	assert.Equal(t, before+1, metricValue(t, "jobs_lease_lost_total", "kind", kind))
}

// TestProcessJobHeartbeatDeadlineCancelsHandler proves repeated transient
// heartbeat failures cancel the handler one heartbeat interval and one
// bookkeeping timeout before the lease can expire, not only once the lease has
// already elapsed (when another worker may have reclaimed the job).
func TestProcessJobHeartbeatDeadlineCancelsHandler(t *testing.T) {
	t.Parallel()

	const kind = "lease.expired"
	fake := &fakeStore{heartbeatErr: errors.New("database unavailable")}
	worker := NewWorker(nil, "expired-worker",
		WithHeartbeatInterval(5*time.Millisecond),
		WithLeaseDuration(10*time.Second),
	)
	worker.store = fake

	// lease 10s - interval 5ms - bookkeeping timeout 5s = 4.995s margin. The
	// fake clock advances one second per read and stops at 5s, so the third
	// failed heartbeat (elapsed 5s) must cancel the handler. The old rule
	// (elapsed >= lease duration) would never fire because the clock stops
	// well below the 10s lease.
	base := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	clock := newFakeClock(base, time.Second, base.Add(5*time.Second))
	worker.now = clock.Now
	require.Equal(t, 4995*time.Millisecond, worker.leaseCancelMargin())

	before := metricValue(t, "jobs_lease_lost_total", "kind", kind)
	cause := make(chan error, 1)
	worker.Register(kind, func(ctx context.Context, _ json.RawMessage) error {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		return ctx.Err()
	})

	require.NoError(t, processJobWithWatchdog(t, worker, context.Background(), claimedJob{
		TenantID:    testID(2),
		ID:          testID(22),
		Kind:        kind,
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	select {
	case got := <-cause:
		assert.ErrorIs(t, got, ErrLeaseLost)
		assert.ErrorContains(t, got, "heartbeat deadline exceeded")
	case <-time.After(time.Second):
		t.Fatal("handler was not canceled after the heartbeat deadline")
	}
	assert.Equal(t, 3, fake.heartbeatCount(), "must cancel once the margin elapsed, before the lease itself")
	assert.Empty(t, fake.succeededIDs())
	assert.Empty(t, fake.failures)
	assert.Empty(t, fake.releases)
	assert.Equal(t, before+1, metricValue(t, "jobs_lease_lost_total", "kind", kind))
}

// TestWorkerLeaseCancelMargin pins the margin formula and its degenerate-lease
// clamp: cancelling must always happen strictly before the lease expires.
func TestWorkerLeaseCancelMargin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		lease    time.Duration
		interval time.Duration
		want     time.Duration
	}{
		{
			name:     "defaults subtract one interval and the bookkeeping timeout",
			lease:    defaultLeaseDuration,
			interval: defaultHeartbeatInterval,
			want:     defaultLeaseDuration - defaultHeartbeatInterval - bookkeepingTimeout,
		},
		{
			name:     "short lease clamps to one further heartbeat",
			lease:    60 * time.Millisecond,
			interval: 5 * time.Millisecond,
			want:     5 * time.Millisecond,
		},
		{
			name:     "lease equal to interval plus bookkeeping clamps to the interval",
			lease:    5*time.Second + time.Millisecond,
			interval: time.Millisecond,
			want:     time.Millisecond,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			worker := NewWorker(nil, "margin-worker",
				WithHeartbeatInterval(tc.interval),
				WithLeaseDuration(tc.lease),
			)
			margin := worker.leaseCancelMargin()
			assert.Equal(t, tc.want, margin)
			assert.Positive(t, margin)
			assert.Less(t, margin, worker.leaseDuration)
		})
	}
}

// TestWorkerCountsLeaseExpiredDeadLetters proves the claim path reports jobs
// dead-lettered by lease expiry instead of leaving them invisible.
func TestWorkerCountsLeaseExpiredDeadLetters(t *testing.T) {
	t.Parallel()

	const kind = "lease.dead"
	fake := &fakeStore{leaseDead: []leaseExpiredJob{{ID: testID(31), TenantID: testID(2), Kind: kind}}}
	worker := NewWorker(nil, "dead-letter-worker", WithPollInterval(time.Millisecond))
	worker.store = fake

	handlerCalls := make(chan struct{}, 1)
	worker.Register(kind, func(context.Context, json.RawMessage) error {
		handlerCalls <- struct{}{}
		return nil
	})

	before := metricValue(t, "jobs_lease_dead_total", "kind", kind)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	require.Eventually(t, func() bool {
		return metricValue(t, "jobs_lease_dead_total", "kind", kind) == before+1
	}, time.Second, time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop")
	}

	select {
	case <-handlerCalls:
		t.Fatal("a lease-expired dead letter must not be dispatched")
	default:
	}
}

func TestPGStoreHeartbeatRequiresPool(t *testing.T) {
	t.Parallel()

	store := newPGStore(nil)
	err := store.Heartbeat(context.Background(), testID(1), "worker", 1, time.Minute)
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
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	_, err := queue.Enqueue(ctx, Job{})
	require.ErrorIs(t, err, ErrEmptyKind)

	_, err = queue.Enqueue(ctx, Job{Kind: "ok", Payload: json.RawMessage(`not-json`)})
	require.ErrorIs(t, err, ErrInvalidPayload)

	_, err = queue.Enqueue(ctx, Job{Kind: "ok"})
	require.ErrorIs(t, err, ErrNoPool)

	_, err = queue.EnqueueTx(ctx, nil, Job{Kind: "ok"})
	require.ErrorIs(t, err, ErrNoTx)

	_, _, err = queue.EnqueueOnce(ctx, Job{Kind: "ok"})
	require.ErrorIs(t, err, ErrEmptyIdempotencyKey)

	_, _, err = queue.EnqueueOnce(ctx, Job{Kind: "ok", IdempotencyKey: " k "})
	require.ErrorIs(t, err, ErrNoPool)

	_, _, err = queue.EnqueueOnceTx(ctx, nil, Job{Kind: "ok", IdempotencyKey: "k"})
	require.ErrorIs(t, err, ErrNoTx)
}

func TestQueuedSenderSendRequiresPool(t *testing.T) {
	t.Parallel()

	codec, codecErr := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, codecErr)
	sender := NewQueuedSender(NewQueue(nil), codec)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	var err error
	require.NotPanics(t, func() {
		err = sender.Send(ctx, email.Message{
			To:      []string{"user@example.com"},
			Subject: "Verify Your Email Address",
		})
	})
	require.ErrorIs(t, err, ErrNoPool)

	// A nil sender or queue must fail cleanly instead of panicking.
	require.NotPanics(t, func() {
		err = (*QueuedSender)(nil).Send(ctx, email.Message{})
	})
	require.Error(t, err)
}

func TestQueuedSenderSendTxRequiresTransaction(t *testing.T) {
	t.Parallel()

	codec, codecErr := NewPayloadCodec(testPayloadSecret)
	require.NoError(t, codecErr)
	sender := NewQueuedSender(NewQueue(nil), codec)
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

func (s *streamingStore) Claim(_ context.Context, _ string, limit int, staleAfter time.Duration) (claimResult, error) {
	s.mu.Lock()
	s.claimCalls++
	s.lastStaleAfter = staleAfter
	s.mu.Unlock()

	if limit <= 0 || len(s.pending) == 0 {
		return claimResult{}, nil
	}
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	batch := append([]claimedJob(nil), s.pending[:limit]...)
	s.pending = s.pending[limit:]
	return claimResult{Jobs: batch}, nil
}

// fakeClock returns a strictly increasing time on every read and stops at max,
// so a heartbeat test can cross the cancel margin without sleeping for a real
// lease duration.
type fakeClock struct {
	mu      sync.Mutex
	current time.Time
	step    time.Duration
	max     time.Time
}

func newFakeClock(start time.Time, step time.Duration, stop time.Time) *fakeClock {
	return &fakeClock{current: start, step: step, max: stop}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.current
	c.current = c.current.Add(c.step)
	if !c.max.IsZero() && c.current.After(c.max) {
		c.current = c.max
	}
	return now
}

// fakeStore records worker outcomes without a database.
type fakeStore struct {
	mu             sync.Mutex
	claimed        []claimedJob
	leaseDead      []leaseExpiredJob
	claimErr       error
	claimErrs      []error
	claimCalls     int
	lastStaleAfter time.Duration
	succeeded      []fakeSuccess
	succeedErr     error
	failures       []fakeFailure
	failErr        error
	releases       []fakeFailure
	heartbeats     []fakeHeartbeat
	heartbeatErr   error
	heartbeatErrs  []error
	purgeCalls     int
	purgeRows      int64
	purgeErr       error
	lastRetention  time.Duration
}

type fakeSuccess struct {
	id       uuid.UUID
	workerID string
	attempts int
}

type fakeFailure struct {
	id       uuid.UUID
	workerID string
	attempts int
	reason   string
	retryAt  *time.Time
}

type fakeHeartbeat struct {
	id       uuid.UUID
	workerID string
	attempts int
}

func (f *fakeStore) Claim(_ context.Context, _ string, _ int, staleAfter time.Duration) (claimResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.claimCalls++
	f.lastStaleAfter = staleAfter
	if len(f.claimErrs) > 0 {
		err := f.claimErrs[0]
		f.claimErrs = f.claimErrs[1:]
		return claimResult{}, err
	}
	if f.claimErr != nil {
		return claimResult{}, f.claimErr
	}
	batch := claimResult{Jobs: f.claimed, LeaseDead: f.leaseDead}
	f.claimed = nil
	f.leaseDead = nil
	return batch, nil
}

func (f *fakeStore) Succeed(_ context.Context, id uuid.UUID, workerID string, attempts int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.succeeded = append(f.succeeded, fakeSuccess{id: id, workerID: workerID, attempts: attempts})
	return f.succeedErr
}

func (f *fakeStore) Fail(_ context.Context, id uuid.UUID, workerID string, attempts int, reason string, retryAt *time.Time) error {
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

func (f *fakeStore) Heartbeat(_ context.Context, id uuid.UUID, workerID string, attempts int, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heartbeats = append(f.heartbeats, fakeHeartbeat{id: id, workerID: workerID, attempts: attempts})
	if len(f.heartbeatErrs) > 0 {
		err := f.heartbeatErrs[0]
		f.heartbeatErrs = f.heartbeatErrs[1:]
		return err
	}
	return f.heartbeatErr
}

func (f *fakeStore) Release(_ context.Context, id uuid.UUID, workerID string, attempts int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, fakeFailure{id: id, workerID: workerID, attempts: attempts, reason: reason})
	return nil
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

func (f *fakeStore) claimCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claimCalls
}

func (f *fakeStore) retentionSeen() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRetention
}

func (f *fakeStore) succeededIDs() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]uuid.UUID, len(f.succeeded))
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

func newJobsTestProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })
	return provider, recorder
}

func TestProcessJobSpanOmitsRawErrors(t *testing.T) {
	t.Parallel()

	provider, recorder := newJobsTestProvider(t)
	fake := &fakeStore{}
	worker := NewWorker(nil, "test-worker", WithTracerProvider(provider))
	worker.store = fake
	worker.retryBase = time.Millisecond
	worker.retryMax = time.Millisecond

	const secret = "smtp rejected user@example.com with token=super-secret"
	worker.Register("fail", func(context.Context, json.RawMessage) error {
		return errors.New(secret)
	})

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID:          testID(11),
		TenantID:    testID(2),
		Kind:        "fail",
		Payload:     json.RawMessage(`{}`),
		Attempts:    1,
		MaxAttempts: 3,
	}))

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	span := ended[0]
	assert.Equal(t, "fail process", span.Name())
	assert.Equal(t, codes.Error, span.Status().Code)
	assert.Empty(t, span.Status().Description)
	assert.Empty(t, span.Events(), "raw error messages must not become span events")
	assert.Equal(t, "application_error", jobsAttrString(span.Attributes(), semconv.ErrorTypeKey))
	assert.Equal(t, "job_queue", jobsAttrString(span.Attributes(), semconv.MessagingSystemKey))
	assert.Equal(t, "process", jobsAttrString(span.Attributes(), semconv.MessagingOperationTypeKey))
	assert.Equal(t, testID(11).String(), jobsAttrString(span.Attributes(), semconv.MessagingMessageIDKey))
	assert.Equal(t, "fail", jobsAttrString(span.Attributes(), semconv.MessagingDestinationNameKey))
	assert.Equal(t, "retried", jobsAttrString(span.Attributes(), attribute.Key("job.outcome")))
	assert.Equal(t, testID(11).String(), jobsAttrString(span.Attributes(), attribute.Key("job.id")))
	assert.Equal(t, "fail", jobsAttrString(span.Attributes(), attribute.Key("job.kind")))
	assert.Equal(t, testID(2).String(), jobsAttrString(span.Attributes(), attribute.Key("job.tenant_id")))
	assert.Equal(t, int64(1), jobsAttrInt(span.Attributes(), attribute.Key("job.attempt")))
	assert.Equal(t, int64(3), jobsAttrInt(span.Attributes(), attribute.Key("job.max_attempts")))

	for _, attr := range span.Attributes() {
		assert.NotContains(t, attr.Value.String(), "user@example.com")
		assert.NotContains(t, attr.Value.String(), "super-secret")
	}
}

func TestProcessJobSpanOutcomeAttributes(t *testing.T) {
	t.Parallel()

	t.Run("succeeded", func(t *testing.T) {
		t.Parallel()
		provider, recorder := newJobsTestProvider(t)
		worker := NewWorker(nil, "test-worker", WithTracerProvider(provider))
		worker.store = &fakeStore{}
		worker.Register("ok", func(context.Context, json.RawMessage) error { return nil })

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID: testID(12), TenantID: testID(1), Kind: "ok", Payload: json.RawMessage(`{}`), Attempts: 1, MaxAttempts: 1,
		}))

		ended := recorder.Ended()
		require.Len(t, ended, 1)
		assert.Equal(t, "succeeded", jobsAttrString(ended[0].Attributes(), attribute.Key("job.outcome")))
		assert.False(t, jobsAttrBool(ended[0].Attributes(), attribute.Key("job.dead_letter")))
	})

	t.Run("dead letter", func(t *testing.T) {
		t.Parallel()
		provider, recorder := newJobsTestProvider(t)
		worker := NewWorker(nil, "test-worker", WithTracerProvider(provider))
		worker.store = &fakeStore{}
		worker.Register("fail", func(context.Context, json.RawMessage) error {
			return errors.New("permanent failure")
		})

		require.NoError(t, worker.processJob(context.Background(), claimedJob{
			ID: testID(13), TenantID: testID(1), Kind: "fail", Payload: json.RawMessage(`{}`), Attempts: 1, MaxAttempts: 1,
		}))

		ended := recorder.Ended()
		require.Len(t, ended, 1)
		attrs := ended[0].Attributes()
		assert.Equal(t, "dead", jobsAttrString(attrs, attribute.Key("job.outcome")))
		assert.True(t, jobsAttrBool(attrs, attribute.Key("job.dead_letter")))
		assert.Empty(t, jobsAttrString(attrs, attribute.Key("job.retry_at")))
		assert.Equal(t, codes.Error, ended[0].Status().Code)
	})
}

func TestProcessJobSpanLinksEnqueueTraceAsNewRoot(t *testing.T) {
	t.Parallel()

	provider, recorder := newJobsTestProvider(t)

	enqueueCtx, enqueueSpan := provider.Tracer("test").Start(context.Background(), "enqueue")
	enqueueSpanCtx := enqueueSpan.SpanContext()
	traceparent := tracing.CaptureRemoteContext(enqueueCtx)
	require.NotEmpty(t, traceparent)
	enqueueSpan.End()

	worker := NewWorker(nil, "test-worker", WithTracerProvider(provider))
	worker.store = &fakeStore{}
	worker.Register("ok", func(context.Context, json.RawMessage) error { return nil })

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID: testID(14), TenantID: testID(1), Kind: "ok", Payload: json.RawMessage(`{}`), Attempts: 1, MaxAttempts: 1,
		TraceParent: traceparent,
	}))

	ended := recorder.Ended()
	require.Len(t, ended, 2)
	jobSpan := ended[1]
	require.Equal(t, "ok process", jobSpan.Name())
	assert.False(t, jobSpan.Parent().IsValid(), "each attempt is a new root")
	require.Len(t, jobSpan.Links(), 1)
	assert.Equal(t, enqueueSpanCtx.TraceID(), jobSpan.Links()[0].SpanContext.TraceID())
	assert.Equal(t, enqueueSpanCtx.SpanID(), jobSpan.Links()[0].SpanContext.SpanID())
}

func TestProcessJobSpanWithoutTraceParentHasNoLink(t *testing.T) {
	t.Parallel()

	provider, recorder := newJobsTestProvider(t)
	worker := NewWorker(nil, "test-worker", WithTracerProvider(provider))
	worker.store = &fakeStore{}
	worker.Register("ok", func(context.Context, json.RawMessage) error { return nil })

	require.NoError(t, worker.processJob(context.Background(), claimedJob{
		ID: testID(15), TenantID: testID(1), Kind: "ok", Payload: json.RawMessage(`{}`), Attempts: 1, MaxAttempts: 1,
		TraceParent: "corrupt-value",
	}))

	ended := recorder.Ended()
	require.Len(t, ended, 1)
	assert.Empty(t, ended[0].Links())
}

func TestJobOutcomeAttributes(t *testing.T) {
	t.Parallel()

	retryAt := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	attrs := jobOutcomeAttributes("retried", &retryAt)
	assert.Equal(t, "retried", jobsAttrString(attrs, attribute.Key("job.outcome")))
	assert.False(t, jobsAttrBool(attrs, attribute.Key("job.dead_letter")))
	assert.Equal(t, retryAt.Format(time.RFC3339), jobsAttrString(attrs, attribute.Key("job.retry_at")))

	attrs = jobOutcomeAttributes("dead", nil)
	assert.True(t, jobsAttrBool(attrs, attribute.Key("job.dead_letter")))
}

func jobsAttrString(attrs []attribute.KeyValue, key attribute.Key) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.AsString()
		}
	}
	return ""
}

func jobsAttrInt(attrs []attribute.KeyValue, key attribute.Key) int64 {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.AsInt64()
		}
	}
	return 0
}

func jobsAttrBool(attrs []attribute.KeyValue, key attribute.Key) bool {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.AsBool()
		}
	}
	return false
}

// testID returns a deterministic UUID for a small fixture number.
func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}
