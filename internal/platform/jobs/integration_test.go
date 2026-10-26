package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// TestStoreIntegrationReleaseGivesAttemptBack proves a shutdown release on the
// final attempt requeues the job with its attempt restored, and that the
// attempts fence still rejects a stale owner.
func TestStoreIntegrationReleaseGivesAttemptBack(t *testing.T) {
	pool := setupIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	id, err := NewQueue(pgtest.Scoped(pool)).Enqueue(ctx, Job{
		Kind:        "jobs.integration.release",
		MaxAttempts: 1,
		RunAt:       time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)

	store := newPGStore(pgtest.Scoped(pool))
	batch, err := store.Claim(ctx, "release-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, batch.Jobs, 1)
	require.Equal(t, 1, batch.Jobs[0].Attempts)

	require.NoError(t, store.Release(ctx, id, "release-worker", 1, "jobs: interrupted by worker shutdown"))
	var (
		status   string
		attempts int
		lockedBy *string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts, locked_by FROM jobs WHERE id = $1`, id,
	).Scan(&status, &attempts, &lockedBy))
	assert.Equal(t, "queued", status, "a final-attempt release must not dead-letter")
	assert.Zero(t, attempts)
	assert.Nil(t, lockedBy)

	assert.ErrorIs(t, store.Release(ctx, id, "release-worker", 1, "stale"), ErrLeaseLost,
		"a released job is no longer owned")

	// The job is claimable again with its full budget.
	batch, err = store.Claim(ctx, "release-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, batch.Jobs, 1)
	assert.Equal(t, 1, batch.Jobs[0].Attempts)
}

// TestWorkerIntegrationProcessesJob exercises Enqueue and Worker against a real
// PostgreSQL server. Without TEST_DATABASE_URL it skips, so the default unit
// test run stays Docker-free.
func TestWorkerIntegrationProcessesJob(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	queue := NewQueue(pgtest.Scoped(pool))
	id, err := queue.Enqueue(ctx, Job{
		Kind:        "jobs.integration.echo",
		Payload:     json.RawMessage(`{"value":42}`),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	received := make(chan string, 1)
	worker := NewWorker(pgtest.Scoped(pool), "integration-worker", WithPollInterval(20*time.Millisecond))
	worker.Register("jobs.integration.echo", func(_ context.Context, payload json.RawMessage) error {
		received <- string(payload)
		return nil
	})
	runIntegrationWorker(t, worker)

	select {
	case payload := <-received:
		assert.JSONEq(t, `{"value":42}`, payload)
	case <-ctx.Done():
		t.Fatal("handler was not invoked")
	}

	var (
		status   string
		attempts int
		lockedBy *string
		lockedAt *time.Time
		payload  []byte
	)
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx,
			`SELECT status, attempts, locked_by, locked_at, payload FROM jobs WHERE id = $1`, id,
		).Scan(&status, &attempts, &lockedBy, &lockedAt, &payload)
		return err == nil && status == "succeeded"
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, 1, attempts)
	assert.Nil(t, lockedBy, "locked_by must be cleared on success")
	assert.Nil(t, lockedAt, "locked_at must be cleared on success")
	assert.JSONEq(t, `{}`, string(payload), "completed jobs must not retain payload secrets")
}

// TestWorkerIntegrationRetriesUntilDead verifies the failure path: attempts are
// incremented per claim, retries are requeued, and the job becomes dead once
// max_attempts is reached.
func TestWorkerIntegrationRetriesUntilDead(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	queue := NewQueue(pgtest.Scoped(pool))
	id, err := queue.Enqueue(ctx, Job{
		Kind:        "jobs.integration.fail",
		Payload:     json.RawMessage(`{}`),
		MaxAttempts: 2,
	})
	require.NoError(t, err)

	worker := NewWorker(pgtest.Scoped(pool), "integration-worker", WithPollInterval(10*time.Millisecond))
	worker.retryBase = 5 * time.Millisecond
	worker.retryMax = 20 * time.Millisecond
	worker.Register("jobs.integration.fail", func(context.Context, json.RawMessage) error {
		return errors.New("always fails")
	})
	runIntegrationWorker(t, worker)

	var (
		status    string
		attempts  int
		lastError *string
		payload   []byte
	)
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx,
			`SELECT status, attempts, last_error, payload FROM jobs WHERE id = $1`, id,
		).Scan(&status, &attempts, &lastError, &payload)
		return err == nil && status == "dead"
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, 2, attempts)
	require.NotNil(t, lastError)
	assert.Contains(t, *lastError, "always fails")
	assert.JSONEq(t, `{}`, string(payload), "dead jobs must not retain payload secrets")
}

// TestWorkerIntegrationUnknownKindRetriesThenDeadLetters verifies that an
// unregistered kind is not dead-lettered immediately: it retries with its
// payload intact, then is dead-lettered with the payload cleared once the
// attempt budget is exhausted.
func TestWorkerIntegrationUnknownKindRetriesThenDeadLetters(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const kind = "jobs.integration.unregistered"
	payload := json.RawMessage(`{"secret":"must-retry-then-clear"}`)
	id, err := NewQueue(pgtest.Scoped(pool)).Enqueue(ctx, Job{
		Kind:        kind,
		Payload:     payload,
		MaxAttempts: 2,
	})
	require.NoError(t, err)

	worker := NewWorker(pgtest.Scoped(pool), "unknown-kind-worker", WithPollInterval(10*time.Millisecond))
	// A long retry delay gives the test a stable window to observe the queued
	// retry before the second claim.
	worker.retryBase = time.Second
	worker.retryMax = time.Second
	// Deliberately register no handler for kind.
	runIntegrationWorker(t, worker)

	var (
		status     string
		attempts   int
		lastError  *string
		gotPayload []byte
	)
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx,
			`SELECT status, attempts, last_error, payload FROM jobs WHERE id = $1`, id,
		).Scan(&status, &attempts, &lastError, &gotPayload)
		return err == nil && status == "queued" && attempts == 1
	}, 5*time.Second, 10*time.Millisecond)

	assert.JSONEq(t, string(payload), string(gotPayload),
		"an unknown kind must retain its payload while retrying")
	require.NotNil(t, lastError)
	assert.Contains(t, *lastError, ErrNoHandler.Error())

	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx,
			`SELECT status, attempts, last_error, payload FROM jobs WHERE id = $1`, id,
		).Scan(&status, &attempts, &lastError, &gotPayload)
		return err == nil && status == "dead"
	}, 5*time.Second, 10*time.Millisecond)

	assert.Equal(t, 2, attempts)
	assert.JSONEq(t, `{}`, string(gotPayload),
		"an unknown kind must be dead-lettered with its payload cleared")
}

// TestClaimIntegrationDeadLettersExpiredFinalAttempt covers a worker that
// crashes after consuming its last allowed attempt. Reclaiming it as queued
// would let a poison job crash-loop forever, so it must become dead.
func TestClaimIntegrationDeadLettersExpiredFinalAttempt(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	id, err := NewQueue(pgtest.Scoped(pool)).Enqueue(ctx, Job{
		Kind:        "jobs.integration.poison",
		Payload:     json.RawMessage(`{"secret":"must-be-cleared"}`),
		MaxAttempts: 1,
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'running', attempts = 1, max_attempts = 1,
		    locked_at = now() - interval '10 minutes', locked_by = 'crashed-worker',
		    lease_expires_at = now() - interval '5 minutes'
		WHERE id = $1`, id)
	require.NoError(t, err)

	store := newPGStore(pgtest.Scoped(pool))
	batch, err := store.Claim(ctx, "recovering-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Empty(t, batch.Jobs, "terminal expired job must not be claimable")
	require.Len(t, batch.LeaseDead, 1, "lease-expiry dead letters must be reported")
	assert.Equal(t, id, batch.LeaseDead[0].ID)
	assert.Equal(t, "jobs.integration.poison", batch.LeaseDead[0].Kind)
	assert.Equal(t, tenant.DefaultID, batch.LeaseDead[0].TenantID)

	var (
		status    string
		payload   []byte
		lastError *string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, payload, last_error FROM jobs WHERE id = $1`, id).Scan(&status, &payload, &lastError))
	assert.Equal(t, "dead", status)
	assert.JSONEq(t, `{}`, string(payload))
	require.NotNil(t, lastError)
	assert.Contains(t, *lastError, "lease expired after max attempts")
}

// TestWorkerIntegrationReclaimsExpiredLease simulates a worker that claimed a
// job and then crashed: the row is running with an old locked_at. A live worker
// must requeue it, claim it again, and process it without consuming an extra
// attempt for the reclaim itself.
func TestWorkerIntegrationReclaimsExpiredLease(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const kind = "jobs.integration.reclaim"
	queue := NewQueue(pgtest.Scoped(pool))
	id, err := queue.Enqueue(ctx, Job{
		Kind:        kind,
		Payload:     json.RawMessage(`{"reclaimed":true}`),
		MaxAttempts: 5,
	})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'running',
		    attempts = 1,
		    locked_at = now() - interval '1 hour',
		    lease_expires_at = now() - interval '55 minutes',
		    locked_by = 'crashed-worker',
		    updated_at = now()
		WHERE id = $1`, id)
	require.NoError(t, err)

	type observation struct {
		attempts  int
		lastError *string
	}
	observed := make(chan observation, 1)

	worker := NewWorker(pgtest.Scoped(pool), "reclaim-worker",
		WithPollInterval(20*time.Millisecond),
		WithLeaseDuration(time.Minute),
	)
	worker.Register(kind, func(ctx context.Context, payload json.RawMessage) error {
		var o observation
		err := pool.QueryRow(ctx,
			`SELECT attempts, last_error FROM jobs WHERE id = $1`, id,
		).Scan(&o.attempts, &o.lastError)
		if err != nil {
			return err
		}
		select {
		case observed <- o:
		default:
		}
		return nil
	})
	runIntegrationWorker(t, worker)

	select {
	case o := <-observed:
		assert.Equal(t, 2, o.attempts,
			"the expired claim holds attempt 1; only the new claim increments it")
		require.NotNil(t, o.lastError)
		assert.Contains(t, *o.lastError, "lease expired")
	case <-ctx.Done():
		t.Fatal("expired lease was not reclaimed and processed")
	}

	var (
		status    string
		attempts  int
		lastError *string
	)
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx,
			`SELECT status, attempts, last_error FROM jobs WHERE id = $1`, id,
		).Scan(&status, &attempts, &lastError)
		return err == nil && status == "succeeded"
	}, 5*time.Second, 20*time.Millisecond)

	assert.Equal(t, 2, attempts)
	assert.Nil(t, lastError, "success clears the lease-expiry marker")
}

// TestSucceedIntegrationFencesStaleAttempt proves that attempts act as a
// fencing token independently of locked_by. The same worker ID reclaims the
// job, so only the attempt predicate can reject its stale writes. It also
// covers Heartbeat and Fail, plus the current owner's ability to finish.
func TestSucceedIntegrationFencesStaleAttempt(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const (
		kind     = "jobs.integration.fence"
		workerID = "fence-worker"
	)
	payload := json.RawMessage(`{"secret":"must-survive"}`)
	id, err := NewQueue(pgtest.Scoped(pool)).Enqueue(ctx, Job{
		Kind:        kind,
		Payload:     payload,
		MaxAttempts: 5,
		RunAt:       time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)

	store := newPGStore(pgtest.Scoped(pool))

	first, err := store.Claim(ctx, workerID, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, first.Jobs, 1)
	require.Equal(t, id, first.Jobs[0].ID)
	require.Equal(t, 1, first.Jobs[0].Attempts)

	// Simulate the expired lease being requeued and reclaimed by the same
	// worker ID. The replacement claim increments attempts, which invalidates
	// the old attempt even though locked_by still matches.
	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'queued',
		    locked_by = NULL,
		    locked_at = NULL,
		    lease_expires_at = NULL,
		    run_at = now()
		WHERE id = $1`, id)
	require.NoError(t, err)

	second, err := store.Claim(ctx, workerID, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, second.Jobs, 1)
	require.Equal(t, id, second.Jobs[0].ID)
	require.Equal(t, 2, second.Jobs[0].Attempts)

	currentAttempt := second.Jobs[0].Attempts
	staleAttempt := first.Jobs[0].Attempts

	// The current owner can heartbeat with its attempt. This also exercises
	// heartbeatJobSQL's $3 parameter order against the database.
	require.NoError(t, store.Heartbeat(ctx, id, workerID, currentAttempt, time.Minute))

	// Every stale bookkeeping write must be rejected by the attempt fence,
	// even though the worker ID still matches.
	require.ErrorIs(t, store.Heartbeat(ctx, id, workerID, staleAttempt, time.Minute), ErrLeaseLost)
	require.ErrorIs(t, store.Fail(ctx, id, workerID, staleAttempt, "stale failure", nil), ErrLeaseLost)
	require.ErrorIs(t, store.Succeed(ctx, id, workerID, staleAttempt), ErrLeaseLost)

	var (
		status     string
		attempts   int
		lockedBy   *string
		lastError  *string
		gotPayload []byte
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts, locked_by, last_error, payload FROM jobs WHERE id = $1`, id,
	).Scan(&status, &attempts, &lockedBy, &lastError, &gotPayload))
	assert.Equal(t, "running", status)
	assert.Equal(t, currentAttempt, attempts)
	require.NotNil(t, lockedBy)
	assert.Equal(t, workerID, *lockedBy)
	assert.Nil(t, lastError, "a fenced stale fail must not write last_error")
	assert.JSONEq(t, string(payload), string(gotPayload),
		"a fenced stale fail must not dead-letter the job or clear its payload")

	// Cleanup: the current owner can still complete the job.
	require.NoError(t, store.Succeed(ctx, id, workerID, currentAttempt))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, payload FROM jobs WHERE id = $1`, id,
	).Scan(&status, &gotPayload))
	assert.Equal(t, "succeeded", status)
	assert.JSONEq(t, `{}`, string(gotPayload), "success still clears the payload")
}

// TestClaimIntegrationDoesNotReclaimHeartbeatedJob holds a row lock the same
// way Heartbeat does while Claim tries to reclaim the row. Claim must wait for
// the heartbeat to commit and then recheck locked_at, leaving the live job
// running instead of stealing it.
func TestClaimIntegrationDoesNotReclaimHeartbeatedJob(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const kind = "jobs.integration.heartbeat"
	id, err := NewQueue(pgtest.Scoped(pool)).Enqueue(ctx, Job{Kind: kind, MaxAttempts: 5})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'running',
		    attempts = 1,
		    locked_at = now() - interval '1 hour',
		    lease_expires_at = now() - interval '55 minutes',
		    locked_by = 'live-worker',
		    updated_at = now()
		WHERE id = $1`, id)
	require.NoError(t, err)

	// Simulate an in-flight heartbeat: the lease deadline moves forward, but
	// the row lock is held until the transaction commits.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE jobs SET locked_at = now(), lease_expires_at = now() + interval '1 minute' WHERE id = $1`, id)
	require.NoError(t, err)

	type claimOutcome struct {
		batch claimResult
		err   error
	}
	claimDone := make(chan claimOutcome, 1)
	go func() {
		batch, err := newPGStore(pgtest.Scoped(pool)).Claim(ctx, "claiming-worker", 1, time.Minute)
		claimDone <- claimOutcome{batch: batch, err: err}
	}()

	// Give Claim time to reach the row lock; committing first would exercise
	// the same fresh-locked_at check without the blocking path.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, tx.Commit(ctx))

	select {
	case result := <-claimDone:
		require.NoError(t, result.err)
		assert.Empty(t, result.batch.Jobs, "a job heartbeated during claim must not be reclaimed")
	case <-ctx.Done():
		t.Fatal("claim did not finish")
	}

	var (
		status   string
		attempts int
		lockedBy *string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts, locked_by FROM jobs WHERE id = $1`, id,
	).Scan(&status, &attempts, &lockedBy))
	assert.Equal(t, "running", status)
	assert.Equal(t, 1, attempts)
	require.NotNil(t, lockedBy)
	assert.Equal(t, "live-worker", *lockedBy)
}

// TestQueueIntegrationEnqueueTx covers the transactional enqueue path,
// including rollback leaving no row behind.
func TestQueueIntegrationEnqueueTx(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	queue := NewQueue(pgtest.Scoped(pool))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	rolledBackID, err := queue.EnqueueTx(ctx, tx, Job{Kind: "jobs.integration.rollback"})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, rolledBackID).Scan(&exists))
	assert.False(t, exists, "rolled-back enqueue must not persist a job")

	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	committedID, err := queue.EnqueueTx(ctx, tx, Job{
		Kind:    "jobs.integration.commit",
		Payload: json.RawMessage(`{"ok":true}`),
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	var (
		status      string
		maxAttempts int
		payload     []byte
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, max_attempts, payload FROM jobs WHERE id = $1`, committedID,
	).Scan(&status, &maxAttempts, &payload))
	assert.Equal(t, "queued", status)
	assert.Equal(t, DefaultMaxAttempts, maxAttempts)
	assert.JSONEq(t, `{"ok":true}`, string(payload))
}

// TestQueueIntegrationEnqueueOnceDeduplicates covers the producer-retry path:
// the same tenant/key returns the original row, a different tenant gets its
// own row, and a rolled-back transactional enqueue leaves nothing behind.
func TestQueueIntegrationEnqueueOnceDeduplicates(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// tenants.id is GENERATED ALWAYS AS IDENTITY: let the database assign it.
	var tenantTwo int64
	err := pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('jobs-once-tenant-2', 'Jobs Once Tenant 2') RETURNING id`,
	).Scan(&tenantTwo)
	require.NoError(t, err)

	queue := NewQueue(pgtest.Scoped(pool))
	key := "event:evt-1:handler:1"
	firstID, created, err := queue.EnqueueOnce(ctx, Job{
		Kind:           "events.test",
		Payload:        json.RawMessage(`{"first":true}`),
		IdempotencyKey: key,
	})
	require.NoError(t, err)
	assert.True(t, created)

	secondID, created, err := queue.EnqueueOnce(ctx, Job{
		Kind:           "events.test",
		Payload:        json.RawMessage(`{"second":true}`),
		IdempotencyKey: key,
	})
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, firstID, secondID)

	var (
		count    int
		payload  []byte
		storedID int64
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*), min(id) FROM jobs WHERE tenant_id = 1 AND idempotency_key = $1`, key,
	).Scan(&count, &storedID))
	assert.Equal(t, 1, count)
	assert.Equal(t, firstID, storedID)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT payload FROM jobs WHERE id = $1`, firstID).Scan(&payload))
	assert.JSONEq(t, `{"first":true}`, string(payload), "deduplication must keep the original payload")

	// The same key in another tenant is independent.
	otherID, created, err := queue.EnqueueOnce(tenant.WithID(ctx, tenantTwo), Job{
		Kind:           "events.test",
		Payload:        json.RawMessage(`{"tenant":2}`),
		IdempotencyKey: key,
	})
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEqual(t, firstID, otherID)

	// A transactional enqueue that rolls back leaves neither job nor dedupe
	// state behind.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, _, err = queue.EnqueueOnceTx(ctx, tx, Job{
		Kind:           "events.test",
		Payload:        json.RawMessage(`{}`),
		IdempotencyKey: "event:rolled-back",
	})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))

	var exists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE idempotency_key = 'event:rolled-back')`,
	).Scan(&exists))
	assert.False(t, exists)

	// The transactional commit path deduplicates exactly like the pool path.
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	txID, created, err := queue.EnqueueOnceTx(ctx, tx, Job{
		Kind:           "events.test",
		Payload:        json.RawMessage(`{}`),
		IdempotencyKey: "event:tx",
	})
	require.NoError(t, err)
	assert.True(t, created)
	require.NoError(t, tx.Commit(ctx))

	tx2, err := pool.Begin(ctx)
	require.NoError(t, err)
	againID, created, err := queue.EnqueueOnceTx(ctx, tx2, Job{
		Kind:           "events.test",
		Payload:        json.RawMessage(`{}`),
		IdempotencyKey: "event:tx",
	})
	require.NoError(t, err)
	require.NoError(t, tx2.Commit(ctx))
	assert.False(t, created)
	assert.Equal(t, txID, againID)
}

// TestQueueIntegrationPropagatesTenant covers tenant resolution at enqueue,
// claim, and handler time: an explicit context tenant is stored on the row and
// restored by the worker, while an empty context falls back to the default
// tenant.
func TestQueueIntegrationPropagatesTenant(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// tenants.id is GENERATED ALWAYS AS IDENTITY: let the database assign it.
	var tenantTwo int64
	err := pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('jobs-tenant-2', 'Jobs Tenant 2') RETURNING id`,
	).Scan(&tenantTwo)
	require.NoError(t, err)

	tenantCtx := tenant.WithID(ctx, tenantTwo)
	queue := NewQueue(pgtest.Scoped(pool))
	store := newPGStore(pgtest.Scoped(pool))

	// Transactional enqueue inherits the context tenant.
	tx, err := pool.Begin(tenantCtx)
	require.NoError(t, err)
	txID, err := queue.EnqueueTx(tenantCtx, tx, Job{
		Kind:  "jobs.integration.tenant.tx",
		RunAt: time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(tenantCtx))

	var storedTenant int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id FROM jobs WHERE id = $1`, txID).Scan(&storedTenant))
	assert.Equal(t, tenantTwo, storedTenant)

	// Claim returns the tenant so the worker can restore it.
	batch, err := store.Claim(tenantCtx, "tenant-claim-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, batch.Jobs, 1)
	assert.Equal(t, txID, batch.Jobs[0].ID)
	assert.Equal(t, tenantTwo, batch.Jobs[0].TenantID)
	require.NoError(t, store.Succeed(tenantCtx, txID, "tenant-claim-worker", batch.Jobs[0].Attempts))

	// An empty context falls back to the compatibility tenant. RunAt is in the
	// past so the immediate claim below does not race a small host/container
	// clock skew on run_at <= now().
	defaultID, err := queue.Enqueue(ctx, Job{
		Kind:  "jobs.integration.tenant.default",
		RunAt: time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id FROM jobs WHERE id = $1`, defaultID).Scan(&storedTenant))
	assert.Equal(t, tenant.DefaultID, storedTenant)

	batch, err = store.Claim(ctx, "default-claim-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, batch.Jobs, 1)
	assert.Equal(t, defaultID, batch.Jobs[0].ID)
	assert.Equal(t, tenant.DefaultID, batch.Jobs[0].TenantID)
	require.NoError(t, store.Succeed(ctx, defaultID, "default-claim-worker", batch.Jobs[0].Attempts))

	// The worker runs the handler with the job's tenant in context.
	const kind = "jobs.integration.tenant.handler"
	received := make(chan int64, 1)
	worker := NewWorker(pgtest.Scoped(pool), "tenant-integration-worker", WithPollInterval(20*time.Millisecond))
	worker.Register(kind, func(ctx context.Context, _ json.RawMessage) error {
		received <- tenant.IDFromContext(ctx)
		return nil
	})
	runIntegrationWorker(t, worker)

	_, err = queue.Enqueue(tenantCtx, Job{Kind: kind, MaxAttempts: 3})
	require.NoError(t, err)

	select {
	case got := <-received:
		assert.Equal(t, tenantTwo, got, "handler must see the claimed job tenant")
	case <-ctx.Done():
		t.Fatal("handler was not invoked")
	}
}

// TestPurgeTerminalIntegration covers the retention delete: only succeeded and
// dead rows older than the window are removed; recent terminal rows and old
// queued/running rows are retained.
func TestPurgeTerminalIntegration(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	insertJob := func(kind, status string, age time.Duration) int64 {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `
			INSERT INTO jobs (kind, payload, status, run_at, max_attempts, created_at, updated_at, lease_expires_at, locked_by)
			VALUES ($1, '{}'::jsonb, $2, now(), 3,
			        now() - make_interval(secs => $3::double precision),
			        now() - make_interval(secs => $3::double precision),
			        CASE WHEN $2 = 'running' THEN now() + interval '1 hour' END,
			        CASE WHEN $2 = 'running' THEN 'live-worker' END)
			RETURNING id`,
			kind, status, age.Seconds(),
		).Scan(&id)
		require.NoError(t, err)
		return id
	}

	oldSucceeded := insertJob("jobs.integration.retention.succeeded", "succeeded", 31*24*time.Hour)
	oldDead := insertJob("jobs.integration.retention.dead", "dead", 31*24*time.Hour)
	recentSucceeded := insertJob("jobs.integration.retention.recent", "succeeded", time.Hour)
	oldQueued := insertJob("jobs.integration.retention.queued", "queued", 31*24*time.Hour)
	oldRunning := insertJob("jobs.integration.retention.running", "running", 31*24*time.Hour)

	purged, err := newPGStore(pgtest.Scoped(pool)).PurgeTerminal(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(2), purged, "only old terminal rows must be deleted")

	exists := func(id int64) bool {
		t.Helper()
		var found bool
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, id).Scan(&found))
		return found
	}

	assert.False(t, exists(oldSucceeded), "an old succeeded job must be purged")
	assert.False(t, exists(oldDead), "an old dead job must be purged")
	assert.True(t, exists(recentSucceeded), "a recent terminal job must be retained")
	assert.True(t, exists(oldQueued), "an old queued job must be retained")
	assert.True(t, exists(oldRunning), "an old running job must be retained")

	// The manual Queue path must count deleted rows too.
	before := metricValue(t, metrics.JobsPurgedTotal)
	manual := insertJob("jobs.integration.retention.manual", "succeeded", 31*24*time.Hour)
	purged, err = NewQueue(pgtest.Scoped(pool)).PurgeTerminal(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), purged)
	assert.False(t, exists(manual))
	assert.Equal(t, before+1, metricValue(t, metrics.JobsPurgedTotal))
}

// TestPurgeTerminalIntegrationBatches proves the purge drains more than one
// batch and stops with a short final batch.
func TestPurgeTerminalIntegrationBatches(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const batchKind = "jobs.integration.retention.batch"
	oldRows := purgeBatchSize + 1
	_, err := pool.Exec(ctx, `
		INSERT INTO jobs (kind, payload, status, run_at, max_attempts, created_at, updated_at)
		SELECT $1, '{}'::jsonb, 'dead', now(), 3,
		       now() - interval '31 days', now() - interval '31 days'
		FROM generate_series(1, $2::int)`,
		batchKind, oldRows,
	)
	require.NoError(t, err)

	var recentID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO jobs (kind, payload, status, run_at, max_attempts, created_at, updated_at)
		VALUES ($1, '{}'::jsonb, 'succeeded', now(), 3, now(), now())
		RETURNING id`, batchKind,
	).Scan(&recentID))

	purged, err := newPGStore(pgtest.Scoped(pool)).PurgeTerminal(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(oldRows), purged)

	var remaining int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = $1`, batchKind).Scan(&remaining))
	assert.Equal(t, int64(1), remaining, "only the recent row must remain")

	var recentExists bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, recentID).Scan(&recentExists))
	assert.True(t, recentExists)
}

// TestQueuedSenderIntegrationEncryptsEmailPayload verifies that the codec
// wiring used by cmd/api stores an encrypted envelope instead of the reset
// link, and that EmailHandler still delivers the decoded message.
func TestQueuedSenderIntegrationEncryptsEmailPayload(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	codec, err := NewPayloadCodec("integration-jwt-secret-at-least-32-characters")
	require.NoError(t, err)

	msg := email.Message{
		To:      []string{"encrypted-email@example.com"},
		Subject: "Reset your password",
		Body:    "Reset your password: https://app.example/reset?token=secret-token",
		HTML:    `<a href="https://app.example/reset?token=secret-token">Reset</a>`,
	}

	sender := NewQueuedSender(NewQueue(pgtest.Scoped(pool)), WithPayloadCodec(codec))
	require.NoError(t, sender.Send(ctx, msg))

	var raw []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT payload FROM jobs WHERE kind = $1 ORDER BY id DESC LIMIT 1`,
		EncryptedEmailJobKind,
	).Scan(&raw))

	encoded := string(raw)
	for _, secret := range []string{"secret-token", "encrypted-email@example.com", "Reset your password"} {
		assert.NotContains(t, encoded, secret, "jobs.payload must not expose email contents")
	}

	var envelope payloadEnvelope
	require.NoError(t, json.Unmarshal(raw, &envelope))
	assert.Equal(t, payloadEnvelopeVersion, envelope.Version)
	assert.NotEmpty(t, envelope.Nonce)
	assert.NotEmpty(t, envelope.Data)

	opened, err := codec.Open(json.RawMessage(raw))
	require.NoError(t, err)
	var decoded email.Message
	require.NoError(t, json.Unmarshal(opened, &decoded))
	assert.Equal(t, msg, decoded)

	delivered := &recordingEmailSender{}
	require.NoError(t, EmailHandler(delivered, WithEmailPayloadCodec(codec))(
		ctx, json.RawMessage(raw),
	))
	require.Len(t, delivered.messages, 1)
	assert.Equal(t, msg, delivered.messages[0])
}

// TestQueuedSenderIntegrationSendTxEncryptsEmailPayload covers the
// transactional enqueue path with the same codec.
func TestQueuedSenderIntegrationSendTxEncryptsEmailPayload(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	codec, err := NewPayloadCodec("integration-jwt-secret-at-least-32-characters")
	require.NoError(t, err)

	msg := email.Message{
		To:      []string{"encrypted-tx@example.com"},
		Subject: "Verify your email address",
		Body:    "Verify: https://app.example/verify?token=tx-secret-token",
	}

	sender := NewQueuedSender(NewQueue(pgtest.Scoped(pool)), WithPayloadCodec(codec))

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, sender.SendTx(ctx, tx, msg))
	require.NoError(t, tx.Commit(ctx))

	var raw []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT payload FROM jobs WHERE kind = $1 ORDER BY id DESC LIMIT 1`,
		EncryptedEmailJobKind,
	).Scan(&raw))

	assert.NotContains(t, string(raw), "tx-secret-token")
	assert.NotContains(t, string(raw), "encrypted-tx@example.com")

	opened, err := codec.Open(json.RawMessage(raw))
	require.NoError(t, err)
	var decoded email.Message
	require.NoError(t, json.Unmarshal(opened, &decoded))
	assert.Equal(t, msg, decoded)
}

// setupIntegrationDB creates an isolated schema, applies the consolidated
// init schema to it, and returns a pool whose connections use that schema.
// TEST_DATABASE_URL must point at a PostgreSQL database where the user may
// create schemas.
func setupIntegrationDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping jobs integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	adminCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	require.NoError(t, admin.Ping(ctx))

	schema := fmt.Sprintf("jobs_test_%d", time.Now().UnixNano())
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+schemaIdent)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE")
	})

	migrations, err := integrationSchema(t)
	require.NoError(t, err)

	conn, err := admin.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	_, err = conn.Exec(ctx, "SET search_path TO "+schemaIdent)
	require.NoError(t, err)
	// One Exec per file, like golang-migrate: a multi-statement string runs in
	// an implicit transaction, which CREATE INDEX CONCURRENTLY rejects.
	for _, migration := range migrations {
		if _, err := conn.Conn().PgConn().Exec(ctx, migration.sql).ReadAll(); err != nil {
			t.Fatalf("apply %s: %v", migration.name, err)
		}
	}

	pools, err := pgtest.OpenAppPools(ctx, dsn, schema)
	require.NoError(t, err)
	t.Cleanup(func() { pgtest.ClosePools(pools) })

	return pools.Bypass
}

// integrationMigration is one up migration file.
type integrationMigration struct {
	name string
	sql  string
}

// integrationSchema returns every up migration in version order, which is
// what golang-migrate applies to a fresh database (one file at a time).
func integrationSchema(t *testing.T) ([]integrationMigration, error) {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve test file path")
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(filename), "..", "..", "..", "migrations", "*.up.sql"))
	if err != nil {
		return nil, err
	}
	require.NotEmpty(t, paths, "no up migrations found")
	sort.Strings(paths)
	migrations := make([]integrationMigration, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, integrationMigration{name: filepath.Base(path), sql: string(raw)})
	}
	return migrations, nil
}

// TestWorkerIntegrationMixedLeasesDoNotStealHealthyJob runs a long-lease worker
// and a short-lease worker on the same kind. The long-lease worker's job is
// healthy (its lease has not expired), so the short-lease worker must never
// reclaim it: expiry is the row's own deadline, not the claimer's lease.
func TestWorkerIntegrationMixedLeasesDoNotStealHealthyJob(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const kind = "jobs.integration.mixed_lease"
	id, err := NewQueue(pgtest.Scoped(pool)).Enqueue(ctx, Job{Kind: kind, MaxAttempts: 5})
	require.NoError(t, err)

	var (
		mu            sync.Mutex
		running       int
		maxConcurrent int
		executions    int
	)
	started := make(chan struct{}, 1)
	handler := func(ctx context.Context, _ json.RawMessage) error {
		mu.Lock()
		running++
		executions++
		maxConcurrent = max(maxConcurrent, running)
		mu.Unlock()
		defer func() {
			mu.Lock()
			running--
			mu.Unlock()
		}()
		select {
		case started <- struct{}{}:
		default:
		}
		// Hold the job far longer than the short worker's lease.
		select {
		case <-time.After(time.Second):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	long := NewWorker(pgtest.Scoped(pool), "long-lease-worker",
		WithPollInterval(10*time.Millisecond),
		WithLeaseDuration(time.Minute),
	)
	long.Register(kind, handler)
	runIntegrationWorker(t, long)

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("long-lease worker did not start the job")
	}

	short := NewWorker(pgtest.Scoped(pool), "short-lease-worker",
		WithPollInterval(10*time.Millisecond),
		WithLeaseDuration(100*time.Millisecond),
		WithHeartbeatInterval(20*time.Millisecond),
	)
	short.Register(kind, handler)
	runIntegrationWorker(t, short)

	var (
		status   string
		attempts int
		lockedBy *string
	)
	require.Eventually(t, func() bool {
		err := pool.QueryRow(ctx,
			`SELECT status, attempts, locked_by FROM jobs WHERE id = $1`, id,
		).Scan(&status, &attempts, &lockedBy)
		return err == nil && status == "succeeded"
	}, 10*time.Second, 20*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, executions, "the healthy job must run exactly once")
	assert.Equal(t, 1, maxConcurrent, "the job must never run on two workers at once")
	assert.Equal(t, 1, attempts, "a reclaim would have burned an attempt")
}

func runIntegrationWorker(t *testing.T, worker *Worker) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop after cancellation")
		}
	})
}

// TestWorkerIntegrationPropagatesTraceParent verifies that the queue persists
// the enqueue-time W3C traceparent and that the worker starts a new root job
// span linked to it. The link, rather than parentage, keeps retries independent
// while preserving causality in the trace UI.
func TestWorkerIntegrationPropagatesTraceParent(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(context.Background())) })

	enqueueCtx, enqueueSpan := provider.Tracer("test").Start(ctx, "enqueue")
	enqueueSnapshot := enqueueSpan.SpanContext()
	traceparent := tracing.CaptureRemoteContext(enqueueCtx)
	require.NotEmpty(t, traceparent)

	queue := NewQueue(pgtest.Scoped(pool))
	id, err := queue.Enqueue(enqueueCtx, Job{
		Kind:        "jobs.integration.traced",
		Payload:     json.RawMessage(`{"value":1}`),
		MaxAttempts: 2,
	})
	require.NoError(t, err)
	enqueueSpan.End()

	var stored *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT trace_parent FROM jobs WHERE id = $1`, id).Scan(&stored))
	require.NotNil(t, stored)
	assert.Equal(t, traceparent, *stored)

	// The deduplicated enqueue path persists the same traceparent.
	onceID, created, err := queue.EnqueueOnce(enqueueCtx, Job{
		Kind:           "jobs.integration.traced.once",
		IdempotencyKey: "trace-once",
	})
	require.NoError(t, err)
	require.True(t, created)
	var onceStored *string
	require.NoError(t, pool.QueryRow(ctx, `SELECT trace_parent FROM jobs WHERE id = $1`, onceID).Scan(&onceStored))
	require.NotNil(t, onceStored)
	assert.Equal(t, traceparent, *onceStored)

	worker := NewWorker(pgtest.Scoped(pool), "trace-integration-worker",
		WithPollInterval(20*time.Millisecond),
		WithTracerProvider(provider),
	)
	worker.Register("jobs.integration.traced", func(context.Context, json.RawMessage) error { return nil })
	worker.Register("jobs.integration.traced.once", func(context.Context, json.RawMessage) error { return nil })
	runIntegrationWorker(t, worker)

	require.Eventually(t, func() bool {
		var status string
		err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, id).Scan(&status)
		return err == nil && status == "succeeded"
	}, 5*time.Second, 20*time.Millisecond)

	require.Eventually(t, func() bool {
		for _, span := range recorder.Ended() {
			if span.Name() == "job.process" {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)

	var jobSpan sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "job.process" {
			jobSpan = span
			break
		}
	}
	require.NotNil(t, jobSpan)
	assert.False(t, jobSpan.Parent().IsValid(), "job.process must be a new root")
	require.Len(t, jobSpan.Links(), 1)
	assert.Equal(t, enqueueSnapshot.TraceID(), jobSpan.Links()[0].SpanContext.TraceID())
	assert.Equal(t, enqueueSnapshot.SpanID(), jobSpan.Links()[0].SpanContext.SpanID())
}
