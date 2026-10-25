package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// claimJobsSQL atomically requeues jobs whose worker lease expired and claims
// due work. The expired CTE releases stale leases before the due CTE selects
// candidates. Expiry is the row's own lease_expires_at, written by the owning
// worker with its own lease duration, so a worker configured with a shorter
// lease never reclaims a healthy job held by a longer-lease worker. $3 is the
// claiming worker's lease and only stamps the rows this claim takes. A
// concurrently committed Heartbeat moves lease_expires_at forward, and
// PostgreSQL rechecks the row after waiting on the heartbeat's row lock, so a
// live job is not reclaimed. Requeued rows become visible to the next claim,
// which is why a call may reclaim a job without returning it.
//
// Reclaiming intentionally leaves attempts unchanged and records the lease
// expiry in last_error; the subsequent claim increments attempts once. A worker
// that dies on its final allowed attempt is dead-lettered instead of being
// requeued forever, which prevents poison jobs from crash-looping. The due CTE
// locks candidate rows with SKIP LOCKED so concurrent workers never claim the
// same job, and the UPDATE increments attempts at claim time.
//
// $4 (include) and $5 (exclude) are optional kind filters set by WithKinds and
// WithExcludeKinds; NULL disables a filter. They only narrow the due CTE: the
// expired-lease CTE stays global so any worker can recover a crashed job of
// any kind (it is merely requeued, never executed by the wrong worker). The
// last column is the queue lag (claim time minus run_at) computed by the
// database clock, so the metric needs no extra query and no clock-skew math.
const claimJobsSQL = `
WITH expired AS (
    UPDATE jobs
    SET status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'queued' END,
        payload = CASE WHEN attempts >= max_attempts THEN '{}'::jsonb ELSE payload END,
        run_at = CASE WHEN attempts >= max_attempts THEN run_at ELSE now() END,
        locked_at = NULL,
        locked_by = NULL,
        lease_expires_at = NULL,
        last_error = CASE WHEN attempts >= max_attempts
            THEN 'jobs: worker lease expired after max attempts'
            ELSE 'jobs: worker lease expired'
        END,
        updated_at = now()
    WHERE status = 'running'
      AND lease_expires_at < now()
    RETURNING id
),
due AS (
    SELECT id
    FROM jobs
    WHERE status = 'queued'
      AND run_at <= now()
      AND ($4::text[] IS NULL OR kind = ANY($4::text[]))
      AND ($5::text[] IS NULL OR kind <> ALL($5::text[]))
    ORDER BY run_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE jobs AS j
SET status = 'running',
    attempts = j.attempts + 1,
    locked_at = now(),
    locked_by = $2,
    lease_expires_at = now() + make_interval(secs => $3::double precision),
    updated_at = now()
FROM due
WHERE j.id = due.id
RETURNING j.id, j.tenant_id, j.kind, j.payload, j.attempts, j.max_attempts, j.trace_parent,
          GREATEST(EXTRACT(EPOCH FROM (now() - j.run_at)), 0)::double precision`

// heartbeatJobSQL renews the lease for a job that is still running under the
// supplied worker and attempt. Matching attempts fences out a stale owner whose
// job was reclaimed at a higher attempt. It extends the deadline by the owning
// worker's lease ($4) and records the heartbeat time in locked_at. A heartbeat
// is not a state transition, so updated_at is deliberately left alone.
const heartbeatJobSQL = `
UPDATE jobs
SET locked_at = now(),
    lease_expires_at = now() + make_interval(secs => $4::double precision)
WHERE id = $1
  AND locked_by = $2
  AND attempts = $3
  AND status = 'running'`

// Completed jobs drop their payload: email jobs can contain password-reset or
// verification links, and a terminal row must not retain usable secrets. The
// attempt fence prevents a worker whose lease was reclaimed from completing the
// job under its new owner.
const succeedJobSQL = `
UPDATE jobs
SET status = 'succeeded',
    payload = '{}'::jsonb,
    locked_at = NULL,
    locked_by = NULL,
    lease_expires_at = NULL,
    last_error = NULL,
    updated_at = now()
WHERE id = $1
  AND locked_by = $2
  AND attempts = $3
  AND status = 'running'`

// failJobSQL requeues the job when retryAt is set and marks it dead when it is
// NULL. locked_by and attempts guard against a worker finishing a job it no
// longer owns; a reclaimed job always carries a higher attempt. Dead jobs drop
// their payload for the same secret-retention reason as success.
const failJobSQL = `
UPDATE jobs
SET status = CASE WHEN $4::timestamptz IS NULL THEN 'dead' ELSE 'queued' END,
    payload = CASE WHEN $4::timestamptz IS NULL THEN '{}'::jsonb ELSE payload END,
    run_at = COALESCE($4::timestamptz, run_at),
    last_error = $3,
    locked_at = NULL,
    locked_by = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = $1
  AND locked_by = $2
  AND attempts = $5
  AND status = 'running'`

// releaseJobSQL requeues a job interrupted by worker shutdown and gives back
// the attempt the claim consumed, so a drain on the final attempt cannot
// dead-letter work that never got a fair run. The same locked_by/attempts
// fence as failJobSQL applies; the next claim increments attempts again.
const releaseJobSQL = `
UPDATE jobs
SET status = 'queued',
    attempts = attempts - 1,
    run_at = now(),
    last_error = $3,
    locked_at = NULL,
    locked_by = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = $1
  AND locked_by = $2
  AND attempts = $4
  AND status = 'running'`

// purgeTerminalJobsSQL deletes one batch of terminal rows older than the
// retention window. Only succeeded and dead jobs are eligible: queued and
// running jobs are live work and must never be removed by retention. The CTE
// limits every statement to purgeBatchSize rows ordered by age, and SKIP LOCKED
// avoids waiting on rows another purge already selected. The window is passed
// in seconds and applied with make_interval so the caller controls it
// precisely.
const purgeTerminalJobsSQL = `
WITH batch AS (
    SELECT id
    FROM jobs
    WHERE status IN ('succeeded', 'dead')
      AND updated_at < now() - make_interval(secs => $1::double precision)
    ORDER BY updated_at
    LIMIT $2
    FOR UPDATE SKIP LOCKED
)
DELETE FROM jobs AS j
USING batch
WHERE j.id = batch.id`

// purgeBatchSize bounds one retention DELETE so a large backlog cannot hold a
// single transaction open past the worker's purge timeout.
const purgeBatchSize = 1000

// pgStore is the PostgreSQL implementation of store.
type pgStore struct {
	db database.DBTX
	// includeKinds and excludeKinds narrow claims; nil means no filter.
	includeKinds []string
	excludeKinds []string
}

var _ store = (*pgStore)(nil)

func newPGStore(db database.DBTX) *pgStore {
	return &pgStore{db: db}
}

// Claim takes up to limit due jobs for workerID and stamps each with a lease
// deadline of now() + lease. Expired leases of any worker are recovered first.
func (s *pgStore) Claim(ctx context.Context, workerID string, limit int, lease time.Duration) ([]claimedJob, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoPool
	}
	if limit <= 0 {
		return nil, nil
	}
	if lease <= 0 {
		lease = defaultLeaseDuration
	}
	ctx = database.WithRLSBypass(ctx) // Claim and lease recovery span every tenant.

	rows, err := s.db.Query(ctx, claimJobsSQL, limit, workerID, lease.Seconds(), s.includeKinds, s.excludeKinds)
	if err != nil {
		return nil, fmt.Errorf("jobs: claim: %w", err)
	}
	defer rows.Close()

	var claimed []claimedJob
	for rows.Next() {
		var (
			job         claimedJob
			payload     string
			traceParent *string
			lagSeconds  float64
		)
		if err := rows.Scan(&job.ID, &job.TenantID, &job.Kind, &payload, &job.Attempts, &job.MaxAttempts, &traceParent, &lagSeconds); err != nil {
			return nil, fmt.Errorf("jobs: scan claimed job: %w", err)
		}
		job.Payload = json.RawMessage(payload)
		job.QueueLag = time.Duration(lagSeconds * float64(time.Second))
		if traceParent != nil {
			job.TraceParent = *traceParent
		}
		claimed = append(claimed, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: claim rows: %w", err)
	}
	return claimed, nil
}

func (s *pgStore) Succeed(ctx context.Context, id int64, workerID string, attempts int) error {
	if s == nil || s.db == nil {
		return ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // The claimed job may belong to any tenant.
	tag, err := s.db.Exec(ctx, succeedJobSQL, id, workerID, attempts)
	if err != nil {
		return fmt.Errorf("jobs: mark %d succeeded: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %d worker %q attempt %d", ErrLeaseLost, id, workerID, attempts)
	}
	return nil
}

func (s *pgStore) Fail(ctx context.Context, id int64, workerID string, attempts int, reason string, retryAt *time.Time) error {
	if s == nil || s.db == nil {
		return ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // The claimed job may belong to any tenant.
	tag, err := s.db.Exec(ctx, failJobSQL, id, workerID, reason, retryAt, attempts)
	if err != nil {
		return fmt.Errorf("jobs: mark %d failed: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %d worker %q attempt %d", ErrLeaseLost, id, workerID, attempts)
	}
	return nil
}

func (s *pgStore) Release(ctx context.Context, id int64, workerID string, attempts int, reason string) error {
	if s == nil || s.db == nil {
		return ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // The claimed job may belong to any tenant.
	tag, err := s.db.Exec(ctx, releaseJobSQL, id, workerID, reason, attempts)
	if err != nil {
		return fmt.Errorf("jobs: release %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %d worker %q attempt %d", ErrLeaseLost, id, workerID, attempts)
	}
	return nil
}

// Heartbeat renews the lease of a running job owned by workerID at attempts. A
// job that was already reclaimed, finished, or claimed by another worker is
// reported as ErrLeaseLost; the caller logs it but leaves the handler running.
// The new deadline is now() + lease, the owning worker's own lease duration.
func (s *pgStore) Heartbeat(ctx context.Context, id int64, workerID string, attempts int, lease time.Duration) error {
	if s == nil || s.db == nil {
		return ErrNoPool
	}
	if lease <= 0 {
		lease = defaultLeaseDuration
	}
	ctx = database.WithRLSBypass(ctx) // The claimed job may belong to any tenant.
	tag, err := s.db.Exec(ctx, heartbeatJobSQL, id, workerID, attempts, lease.Seconds())
	if err != nil {
		return fmt.Errorf("jobs: heartbeat %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: job %d worker %q attempt %d", ErrLeaseLost, id, workerID, attempts)
	}
	return nil
}

// PurgeTerminal deletes succeeded and dead jobs whose last update is older than
// olderThan and returns the number of deleted rows. Deletion happens in bounded
// batches until a batch is short (the backlog is drained) or the context ends.
// If a later batch fails, the successfully deleted total is returned alongside
// the error. Queued and running jobs are never eligible, regardless of age.
func (s *pgStore) PurgeTerminal(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, ErrInvalidRetention
	}
	if s == nil || s.db == nil {
		return 0, ErrNoPool
	}
	ctx = database.WithRLSBypass(ctx) // Retention deletes terminal jobs of every tenant.

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("jobs: purge terminal jobs: %w", err)
		}
		tag, err := s.db.Exec(ctx, purgeTerminalJobsSQL, olderThan.Seconds(), purgeBatchSize)
		if err != nil {
			return total, fmt.Errorf("jobs: purge terminal jobs: %w", err)
		}
		deleted := tag.RowsAffected()
		total += deleted
		if deleted < purgeBatchSize {
			return total, nil
		}
	}
}
