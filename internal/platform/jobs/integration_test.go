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
// fencing token. A worker whose lease was reclaimed cannot finish the job:
// the replacement claim owns a higher attempt, so the old worker's Succeed
// affects no rows and must report ErrLeaseLost.
func TestSucceedIntegrationFencesStaleAttempt(t *testing.T) {
	pool := setupIntegrationDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const kind = "jobs.integration.fence"
	id, err := NewQueue(pool).Enqueue(ctx, Job{
		Kind:        kind,
		Payload:     json.RawMessage(`{"secret":"must-survive"}`),
		MaxAttempts: 5,
	})
	require.NoError(t, err)

	store := newPGStore(pool)

	first, err := store.Claim(ctx, "w1", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, id, first[0].ID)
	require.Equal(t, 1, first[0].Attempts)

	// Simulate the expired lease being requeued and reclaimed by w2. The
	// replacement claim increments attempts, which invalidates w1's fence.
	_, err = pool.Exec(ctx, `
		UPDATE jobs
		SET status = 'queued',
		    locked_by = NULL,
		    locked_at = NULL,
		    run_at = now()
		WHERE id = $1`, id)
	require.NoError(t, err)

	second, err := store.Claim(ctx, "w2", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, id, second[0].ID)
	require.Equal(t, 2, second[0].Attempts)

	err = store.Succeed(ctx, id, "w1", first[0].Attempts)
	require.ErrorIs(t, err, ErrLeaseLost,
		"the stale attempt must not be allowed to complete a reclaimed job")

	var (
		status   string
		attempts int
		lockedBy *string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, attempts, locked_by FROM jobs WHERE id = $1`, id,
	).Scan(&status, &attempts, &lockedBy))
	assert.Equal(t, "running", status)
	assert.Equal(t, 2, attempts)
	require.NotNil(t, lockedBy)
	assert.Equal(t, "w2", *lockedBy)
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
