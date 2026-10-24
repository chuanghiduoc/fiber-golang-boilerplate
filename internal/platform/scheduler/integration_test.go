package scheduler

import (
	"context"
	"encoding/json"
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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

func TestSchedulerIntegrationLeaderLease(t *testing.T) {
	pool := setupSchedulerIntegrationDB(t)
	store := newPGStore(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	acquired, err := store.AcquireLease(ctx, "node-a", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired)

	acquired, err = store.AcquireLease(ctx, "node-b", time.Minute)
	require.NoError(t, err)
	assert.False(t, acquired, "a live lease must not be stolen")

	acquired, err = store.AcquireLease(ctx, "node-a", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired, "the holder may renew")

	require.NoError(t, store.ReleaseLease(ctx, "node-a"))
	acquired, err = store.AcquireLease(ctx, "node-b", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired, "a released lease may be taken")

	// An expired lease is recoverable even if the old process never released.
	_, err = pool.Exec(ctx,
		`UPDATE scheduler_leases SET expires_at = now() - interval '1 minute' WHERE name = $1`, leaseName)
	require.NoError(t, err)
	acquired, err = store.AcquireLease(ctx, "node-c", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired)
}

func TestSchedulerIntegrationRunLedgerAndEnqueue(t *testing.T) {
	pool := setupSchedulerIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const tenantTwo int64 = 2
	_, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, slug, name) VALUES ($1, 'scheduler-tenant-2', 'Scheduler Tenant 2')`,
		tenantTwo,
	)
	require.NoError(t, err)

	store := newPGStore(pool)
	queue := jobs.NewQueue(pool)
	slot := time.Date(2026, 10, 24, 12, 5, 0, 0, time.UTC)
	run := runRecord{Schedule: "cleanup", ScheduledFor: slot, Status: runStatusEnqueued}
	job := jobs.Job{
		TenantID:       tenantTwo,
		Kind:           "maintenance.cleanup",
		Payload:        json.RawMessage(`{"schedule":"cleanup","scheduledFor":"2026-10-24T12:05:00Z"}`),
		MaxAttempts:    3,
		IdempotencyKey: "scheduler:cleanup:2026-10-24T12:05:00Z",
	}

	created, err := store.EnqueueRun(ctx, queue, run, job)
	require.NoError(t, err)
	assert.True(t, created)

	created, err = store.EnqueueRun(ctx, queue, run, job)
	require.NoError(t, err)
	assert.False(t, created, "the unique ledger must suppress a duplicate slot")

	last, ok, err := store.LastRun(ctx, "cleanup")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, slot, last)

	var (
		runCount int
		jobCount int
		jobID    int64
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM scheduler_runs WHERE schedule = 'cleanup'`).Scan(&runCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*), min(id) FROM jobs WHERE kind = 'maintenance.cleanup'`).Scan(&jobCount, &jobID))
	assert.Equal(t, 1, runCount)
	assert.Equal(t, 1, jobCount)

	var (
		storedTenant int64
		storedKey    string
		payload      []byte
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id, idempotency_key, payload FROM jobs WHERE id = $1`, jobID,
	).Scan(&storedTenant, &storedKey, &payload))
	assert.Equal(t, tenantTwo, storedTenant)
	assert.Equal(t, job.IdempotencyKey, storedKey)
	assert.JSONEq(t, string(job.Payload), string(payload))

	// A skipped slot is also fenced by the same unique ledger.
	skipped := runRecord{Schedule: "cleanup", ScheduledFor: slot.Add(5 * time.Minute), Status: runStatusSkipped}
	created, err = store.SkipRun(ctx, skipped)
	require.NoError(t, err)
	assert.True(t, created)
	created, err = store.SkipRun(ctx, skipped)
	require.NoError(t, err)
	assert.False(t, created)

	// Retention deletes only old ledger rows.
	_, err = pool.Exec(ctx, `
		INSERT INTO scheduler_runs (schedule, scheduled_for, status, created_at)
		VALUES ('cleanup', $1, 'skipped', now() - interval '31 days')`,
		slot.Add(-24*time.Hour))
	require.NoError(t, err)
	purged, err := store.PurgeRuns(ctx, 30*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, int64(1), purged)
}

func TestSchedulerIntegrationSingleLeaderEnqueuesOnce(t *testing.T) {
	pool := setupSchedulerIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	registry := NewRegistry()
	require.NoError(t, registry.Register(Spec{
		Name:     "integration.every-minute",
		Interval: time.Minute,
		Kind:     "integration.scheduler.tick",
		TenantID: tenant.DefaultID,
	}))
	queue := jobs.NewQueue(pool)

	first := New(pool, registry, queue, "integration-node-a",
		WithPollInterval(10*time.Millisecond),
		WithHeartbeatInterval(50*time.Millisecond),
		WithLeaseTTL(time.Second),
		WithCatchup(CatchupLatest),
	)
	second := New(pool, registry, queue, "integration-node-b",
		WithPollInterval(10*time.Millisecond),
		WithHeartbeatInterval(50*time.Millisecond),
		WithLeaseTTL(time.Second),
		WithCatchup(CatchupLatest),
	)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 2)
	go func() { done <- first.Run(runCtx) }()
	go func() { done <- second.Run(runCtx) }()

	require.Eventually(t, func() bool {
		var count int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM jobs WHERE kind = 'integration.scheduler.tick'`).Scan(&count)
		return err == nil && count >= 1
	}, 5*time.Second, 20*time.Millisecond)

	// Give the loser time to poll and confirm it cannot enqueue the same slot.
	time.Sleep(150 * time.Millisecond)
	stop()
	for range 2 {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("scheduler did not stop")
		}
	}

	var (
		jobCount int
		runCount int
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'integration.scheduler.tick'`).Scan(&jobCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM scheduler_runs WHERE schedule = 'integration.every-minute'`).Scan(&runCount))
	assert.Equal(t, 1, jobCount, "only one scheduler slot may be enqueued")
	assert.Equal(t, 1, runCount)
}

func setupSchedulerIntegrationDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping scheduler integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	adminCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	require.NoError(t, admin.Ping(ctx))

	schema := fmt.Sprintf("scheduler_test_%d", time.Now().UnixNano())
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+schemaIdent)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE")
	})

	migration, err := os.ReadFile(schedulerIntegrationMigrationPath(t))
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

func schedulerIntegrationMigrationPath(t *testing.T) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve test file path")
	return filepath.Join(filepath.Dir(filename), "..", "..", "..", "migrations", "000001_init_schema.up.sql")
}
