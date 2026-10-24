package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// TestWorkerIntegrationProcessesJob exercises Enqueue and Worker against a real
// PostgreSQL server. Without TEST_DATABASE_URL it skips, so the default unit
// test run stays Docker-free.
func TestWorkerIntegrationProcessesJob(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	queue := NewQueue(pool)
	id, err := queue.Enqueue(ctx, Job{
		Kind:        "jobs.integration.echo",
		Payload:     json.RawMessage(`{"value":42}`),
		MaxAttempts: 3,
	})
	require.NoError(t, err)

	received := make(chan string, 1)
	worker := NewWorker(pool, "integration-worker", WithPollInterval(20*time.Millisecond))
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

	queue := NewQueue(pool)
	id, err := queue.Enqueue(ctx, Job{
		Kind:        "jobs.integration.fail",
		Payload:     json.RawMessage(`{}`),
		MaxAttempts: 2,
	})
	require.NoError(t, err)

	worker := NewWorker(pool, "integration-worker", WithPollInterval(10*time.Millisecond))
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
	id, err := NewQueue(pool).Enqueue(ctx, Job{
		Kind:        kind,
		Payload:     payload,
		MaxAttempts: 2,
	})
	require.NoError(t, err)

	worker := NewWorker(pool, "unknown-kind-worker", WithPollInterval(10*time.Millisecond))
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

	id, err := NewQueue(pool).Enqueue(ctx, Job{
		Kind:        "jobs.integration.poison",
		Payload:     json.RawMessage(`{"secret":"must-be-cleared"}`),
		MaxAttempts: 1,
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'running', attempts = 1, max_attempts = 1,
		    locked_at = now() - interval '10 minutes', locked_by = 'crashed-worker'
		WHERE id = $1`, id)
	require.NoError(t, err)

	store := newPGStore(pool)
	claimed, err := store.Claim(ctx, "recovering-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Empty(t, claimed, "terminal expired job must not be claimable")

	var (
		status  string
		payload []byte
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, payload FROM jobs WHERE id = $1`, id).Scan(&status, &payload))
	assert.Equal(t, "dead", status)
	assert.JSONEq(t, `{}`, string(payload))
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
	queue := NewQueue(pool)
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
		    locked_by = 'crashed-worker',
		    updated_at = now()
		WHERE id = $1`, id)
	require.NoError(t, err)

	type observation struct {
		attempts  int
		lastError *string
	}
	observed := make(chan observation, 1)

	worker := NewWorker(pool, "reclaim-worker",
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
	id, err := NewQueue(pool).Enqueue(ctx, Job{
		Kind:        kind,
		Payload:     payload,
		MaxAttempts: 5,
	})
	require.NoError(t, err)

	store := newPGStore(pool)

	first, err := store.Claim(ctx, workerID, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, id, first[0].ID)
	require.Equal(t, 1, first[0].Attempts)

	// Simulate the expired lease being requeued and reclaimed by the same
	// worker ID. The replacement claim increments attempts, which invalidates
	// the old attempt even though locked_by still matches.
	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'queued',
		    locked_by = NULL,
		    locked_at = NULL,
		    run_at = now()
		WHERE id = $1`, id)
	require.NoError(t, err)

	second, err := store.Claim(ctx, workerID, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, id, second[0].ID)
	require.Equal(t, 2, second[0].Attempts)

	currentAttempt := second[0].Attempts
	staleAttempt := first[0].Attempts

	// The current owner can heartbeat with its attempt. This also exercises
	// heartbeatJobSQL's $3 parameter order against the database.
	require.NoError(t, store.Heartbeat(ctx, id, workerID, currentAttempt))

	// Every stale bookkeeping write must be rejected by the attempt fence,
	// even though the worker ID still matches.
	require.ErrorIs(t, store.Heartbeat(ctx, id, workerID, staleAttempt), ErrLeaseLost)
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
	id, err := NewQueue(pool).Enqueue(ctx, Job{Kind: kind, MaxAttempts: 5})
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'running',
		    attempts = 1,
		    locked_at = now() - interval '1 hour',
		    locked_by = 'live-worker',
		    updated_at = now()
		WHERE id = $1`, id)
	require.NoError(t, err)

	// Simulate an in-flight heartbeat: locked_at moves forward, but the row
	// lock is held until the transaction commits.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `UPDATE jobs SET locked_at = now() WHERE id = $1`, id)
	require.NoError(t, err)

	type claimResult struct {
		jobs []claimedJob
		err  error
	}
	claimDone := make(chan claimResult, 1)
	go func() {
		jobs, err := newPGStore(pool).Claim(ctx, "claiming-worker", 1, time.Minute)
		claimDone <- claimResult{jobs: jobs, err: err}
	}()

	// Give Claim time to reach the row lock; committing first would exercise
	// the same fresh-locked_at check without the blocking path.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, tx.Commit(ctx))

	select {
	case result := <-claimDone:
		require.NoError(t, result.err)
		assert.Empty(t, result.jobs, "a job heartbeated during claim must not be reclaimed")
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

	queue := NewQueue(pool)

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

// TestQueueIntegrationPropagatesTenant covers tenant resolution at enqueue,
// claim, and handler time: an explicit context tenant is stored on the row and
// restored by the worker, while an empty context falls back to the default
// tenant.
func TestQueueIntegrationPropagatesTenant(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const tenantTwo int64 = 2
	_, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, 'jobs-tenant-2', 'Jobs Tenant 2')`,
		tenantTwo,
	)
	require.NoError(t, err)

	tenantCtx := tenant.WithID(ctx, tenantTwo)
	queue := NewQueue(pool)
	store := newPGStore(pool)

	// Transactional enqueue inherits the context tenant.
	tx, err := pool.Begin(tenantCtx)
	require.NoError(t, err)
	txID, err := queue.EnqueueTx(tenantCtx, tx, Job{Kind: "jobs.integration.tenant.tx"})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(tenantCtx))

	var storedTenant int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id FROM jobs WHERE id = $1`, txID).Scan(&storedTenant))
	assert.Equal(t, tenantTwo, storedTenant)

	// Claim returns the tenant so the worker can restore it.
	claimed, err := store.Claim(tenantCtx, "tenant-claim-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, txID, claimed[0].ID)
	assert.Equal(t, tenantTwo, claimed[0].TenantID)
	require.NoError(t, store.Succeed(tenantCtx, txID, "tenant-claim-worker", claimed[0].Attempts))

	// An empty context falls back to the compatibility tenant.
	defaultID, err := queue.Enqueue(ctx, Job{Kind: "jobs.integration.tenant.default"})
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id FROM jobs WHERE id = $1`, defaultID).Scan(&storedTenant))
	assert.Equal(t, tenant.DefaultID, storedTenant)

	claimed, err = store.Claim(ctx, "default-claim-worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, defaultID, claimed[0].ID)
	assert.Equal(t, tenant.DefaultID, claimed[0].TenantID)
	require.NoError(t, store.Succeed(ctx, defaultID, "default-claim-worker", claimed[0].Attempts))

	// The worker runs the handler with the job's tenant in context.
	const kind = "jobs.integration.tenant.handler"
	received := make(chan int64, 1)
	worker := NewWorker(pool, "tenant-integration-worker", WithPollInterval(20*time.Millisecond))
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
			INSERT INTO jobs (kind, payload, status, run_at, max_attempts, created_at, updated_at)
			VALUES ($1, '{}'::jsonb, $2, now(), 3,
			        now() - make_interval(secs => $3::double precision),
			        now() - make_interval(secs => $3::double precision))
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

	purged, err := newPGStore(pool).PurgeTerminal(ctx, 30*24*time.Hour)
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
	purged, err = NewQueue(pool).PurgeTerminal(ctx, 30*24*time.Hour)
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

	purged, err := newPGStore(pool).PurgeTerminal(ctx, 30*24*time.Hour)
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

	sender := NewQueuedSender(NewQueue(pool), WithPayloadCodec(codec))
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

	sender := NewQueuedSender(NewQueue(pool), WithPayloadCodec(codec))

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

	migration, err := os.ReadFile(integrationMigrationPath(t))
	require.NoError(t, err)

	conn, err := admin.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	_, err = conn.Exec(ctx, "SET search_path TO "+schemaIdent)
	require.NoError(t, err)
	if _, err := conn.Conn().PgConn().Exec(ctx, string(migration)).ReadAll(); err != nil {
		t.Fatalf("apply migrations/000001_init_schema.up.sql: %v", err)
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	return pool
}

func integrationMigrationPath(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve test file path")
	return filepath.Join(filepath.Dir(filename), "..", "..", "..", "migrations", "000001_init_schema.up.sql")
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
