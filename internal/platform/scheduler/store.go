package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
)

var (
	// ErrNoPool is returned when a scheduler has no database pool.
	ErrNoPool = errors.New("scheduler: database pool is nil")
	// ErrNoQueue is returned when a scheduler has no durable queue.
	ErrNoQueue = errors.New("scheduler: job queue is nil")
	// ErrInvalidRetention is returned when a retention window is not positive.
	ErrInvalidRetention = errors.New("scheduler: retention must be positive")
)

const (
	leaseName          = "scheduler"
	purgeRunsBatchSize = 1000
)

// Queue is the durable jobs surface used by the scheduler. *jobs.Queue
// implements it.
type Queue interface {
	EnqueueOnceTx(ctx context.Context, tx pgx.Tx, job jobs.Job) (uuid.UUID, bool, error)
}

type runRecord struct {
	Schedule     string
	ScheduledFor time.Time
	Status       string
	Error        string
}

const (
	runStatusEnqueued = "enqueued"
	runStatusSkipped  = "skipped"
)

// store is the PostgreSQL surface used by Scheduler. It is an interface so the
// leader loop, catch-up decisions, and error paths can be tested with a fake.
type store interface {
	AcquireLease(ctx context.Context, holder string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, holder string) error
	LastRun(ctx context.Context, schedule string) (time.Time, bool, error)
	EnqueueRun(ctx context.Context, queue Queue, run runRecord, job jobs.Job) (bool, error)
	SkipRun(ctx context.Context, run runRecord) (bool, error)
	PurgeRuns(ctx context.Context, olderThan time.Duration) (int64, error)
}

// acquireLeaseSQL takes or renews the single scheduler lease. A live lease
// owned by another holder is never stolen; an expired lease can be taken even
// if the previous process is still unwinding, which is the crash-recovery path.
const acquireLeaseSQL = `
INSERT INTO scheduler_leases (name, holder, expires_at, heartbeat_at, created_at, updated_at)
VALUES ($1, $2, now() + make_interval(secs => $3::double precision), now(), now(), now())
ON CONFLICT (name) DO UPDATE
SET holder = EXCLUDED.holder,
    expires_at = EXCLUDED.expires_at,
    heartbeat_at = now(),
    updated_at = now()
WHERE scheduler_leases.holder = EXCLUDED.holder
   OR scheduler_leases.expires_at < now()
RETURNING holder`

const releaseLeaseSQL = `
UPDATE scheduler_leases
SET expires_at = now(),
    heartbeat_at = now(),
    updated_at = now()
WHERE name = $1 AND holder = $2`

const lastRunSQL = `
SELECT scheduled_for
FROM scheduler_runs
WHERE schedule = $1
ORDER BY scheduled_for DESC
LIMIT 1`

const insertRunSQL = `
INSERT INTO scheduler_runs (schedule, scheduled_for, status, error, created_at)
VALUES ($1, $2, $3, NULLIF($4, ''), now())
ON CONFLICT (schedule, scheduled_for) DO NOTHING
RETURNING id`

const setRunJobSQL = `
UPDATE scheduler_runs SET job_id = $1 WHERE id = $2`

// purgeRunsSQL deletes one batch of old ledger rows but never a schedule's
// newest slot: LastRun anchors catch-up on that row, so deleting it would make
// the schedule look never-run and re-enqueue an already executed slot. The
// EXISTS probe uses idx_scheduler_runs_schedule_slot.
const purgeRunsSQL = `
WITH batch AS (
    SELECT id
    FROM scheduler_runs AS candidate
    WHERE created_at < now() - make_interval(secs => $1::double precision)
      AND EXISTS (
          SELECT 1
          FROM scheduler_runs AS newer
          WHERE newer.schedule = candidate.schedule
            AND newer.scheduled_for > candidate.scheduled_for
      )
    ORDER BY created_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
DELETE FROM scheduler_runs AS r
USING batch
WHERE r.id = batch.id`

// pgStore reads the scheduler's own coordination tables from pool. Those have
// no tenant column, so no RLS scope applies; run transactions go through tx so
// the job enqueued inside them carries the right scope.
type pgStore struct {
	pool *pgxpool.Pool
	tx   *database.TxManager
}

var _ store = (*pgStore)(nil)

func newPGStore(pool *pgxpool.Pool, tx *database.TxManager) *pgStore {
	return &pgStore{pool: pool, tx: tx}
}

func (s *pgStore) AcquireLease(ctx context.Context, holder string, ttl time.Duration) (bool, error) {
	if s == nil || s.pool == nil {
		return false, ErrNoPool
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	var got string
	err := s.pool.QueryRow(ctx, acquireLeaseSQL, leaseName, holder, ttl.Seconds()).Scan(&got)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("scheduler: acquire lease: %w", err)
	}
}

func (s *pgStore) ReleaseLease(ctx context.Context, holder string) error {
	if s == nil || s.pool == nil {
		return ErrNoPool
	}
	if _, err := s.pool.Exec(ctx, releaseLeaseSQL, leaseName, holder); err != nil {
		return fmt.Errorf("scheduler: release lease: %w", err)
	}
	return nil
}

func (s *pgStore) LastRun(ctx context.Context, schedule string) (time.Time, bool, error) {
	if s == nil || s.pool == nil {
		return time.Time{}, false, ErrNoPool
	}
	var slot time.Time
	err := s.pool.QueryRow(ctx, lastRunSQL, schedule).Scan(&slot)
	switch {
	case err == nil:
		return slot.UTC(), true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	default:
		return time.Time{}, false, fmt.Errorf("scheduler: last run for %q: %w", schedule, err)
	}
}

// EnqueueRun records the slot and enqueues its job in one transaction. The
// unique ledger insert is the first statement: a duplicate slot rolls back
// without enqueueing. A queue failure rolls the ledger row back too, so the
// next poll retries the slot.
func (s *pgStore) EnqueueRun(ctx context.Context, queue Queue, run runRecord, job jobs.Job) (bool, error) {
	if s == nil || s.pool == nil {
		return false, ErrNoPool
	}
	if queue == nil {
		return false, ErrNoQueue
	}

	ctx = database.WithRLSBypass(ctx) // Schedules enqueue global platform jobs, not a tenant's.
	enqueued := false
	err := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var runID uuid.UUID
		err := tx.QueryRow(ctx, insertRunSQL, run.Schedule, run.ScheduledFor, run.Status, run.Error).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scheduler: record run for %q: %w", run.Schedule, err)
		}

		jobID, _, err := queue.EnqueueOnceTx(ctx, tx, job)
		if err != nil {
			return fmt.Errorf("scheduler: enqueue %q for %q: %w", job.Kind, run.Schedule, err)
		}
		if _, err := tx.Exec(ctx, setRunJobSQL, jobID, runID); err != nil {
			return fmt.Errorf("scheduler: attach job to run for %q: %w", run.Schedule, err)
		}
		enqueued = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return enqueued, nil
}

func (s *pgStore) SkipRun(ctx context.Context, run runRecord) (bool, error) {
	if s == nil || s.pool == nil {
		return false, ErrNoPool
	}
	skipped := false
	err := s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		var runID uuid.UUID
		err := tx.QueryRow(ctx, insertRunSQL, run.Schedule, run.ScheduledFor, run.Status, run.Error).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("scheduler: record skipped run for %q: %w", run.Schedule, err)
		}
		skipped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return skipped, nil
}

func (s *pgStore) PurgeRuns(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, ErrInvalidRetention
	}
	if s == nil || s.pool == nil {
		return 0, ErrNoPool
	}

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("scheduler: purge runs: %w", err)
		}
		tag, err := s.pool.Exec(ctx, purgeRunsSQL, olderThan.Seconds(), purgeRunsBatchSize)
		if err != nil {
			return total, fmt.Errorf("scheduler: purge runs: %w", err)
		}
		deleted := tag.RowsAffected()
		total += deleted
		if deleted < purgeRunsBatchSize {
			return total, nil
		}
	}
}
