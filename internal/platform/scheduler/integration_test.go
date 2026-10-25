package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

	// tenants.id is GENERATED ALWAYS AS IDENTITY: let the database assign it.
	var tenantTwo int64
	err := pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('scheduler-tenant-2', 'Scheduler Tenant 2') RETURNING id`,
	).Scan(&tenantTwo)
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

// TestSchedulerIntegrationPurgeKeepsLatestSlot reproduces a ledger purge that
// deleted a sparse schedule's only (newest) row: the scheduler then treated the
// schedule as never-run and re-enqueued the already executed slot.
func TestSchedulerIntegrationPurgeKeepsLatestSlot(t *testing.T) {
	pool := setupSchedulerIntegrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const name = "integration.yearly"
	_, err := pool.Exec(ctx, `
		INSERT INTO scheduler_runs (schedule, scheduled_for, status, created_at)
		VALUES ($1, '2025-01-01T00:00:00Z', 'enqueued', now() - interval '400 days'),
		       ($1, '2026-01-01T00:00:00Z', 'enqueued', now() - interval '60 days')`,
		name)
	require.NoError(t, err)

	registry := NewRegistry()
	require.NoError(t, registry.Register(Spec{
		Name:     name,
		Cron:     "@yearly",
		Kind:     "integration.scheduler.yearly",
		TenantID: tenant.DefaultID,
	}))
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	sched := New(pool, registry, jobs.NewQueue(pool), "integration-node-purge",
		WithPollInterval(10*time.Millisecond),
		WithHeartbeatInterval(50*time.Millisecond),
		WithLeaseTTL(time.Second),
		WithCatchup(CatchupLatest),
		WithRetention(30*24*time.Hour),
		WithPurgeInterval(time.Nanosecond),
		WithClock(func() time.Time { return now }),
	)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- sched.Run(runCtx) }()

	require.Eventually(t, func() bool {
		var count int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM scheduler_runs WHERE schedule = $1`, name).Scan(&count)
		return err == nil && count == 1
	}, 5*time.Second, 20*time.Millisecond, "the older row must be purged")
	// Let several poll+purge cycles pass on the purged ledger.
	time.Sleep(200 * time.Millisecond)
	stop()
	require.NoError(t, <-done)

	var (
		slot     time.Time
		jobCount int
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT scheduled_for FROM scheduler_runs WHERE schedule = $1`, name).Scan(&slot))
	assert.True(t, slot.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), "the newest slot must survive retention")
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'integration.scheduler.yearly'`).Scan(&jobCount))
	assert.Zero(t, jobCount, "an already executed slot must not be re-enqueued")
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

	poolCfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, pool.Ping(ctx))

	return pool
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
