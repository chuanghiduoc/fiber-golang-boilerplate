package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tracing"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

// insertJobSQL inserts one queued job and returns its id. The cast makes the
// parameter type explicit so a JSON document is never mistaken for bytea.
const insertJobSQL = `
INSERT INTO jobs (tenant_id, kind, payload, trace_parent, status, run_at, max_attempts, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3::jsonb, NULLIF($4, ''), 'queued', $5, $6, $7, now(), now())
RETURNING id`

// enqueueOnceSQL inserts a job unless the same tenant/key is already present.
// The conflict target matches idx_jobs_tenant_idempotency_key, including its
// partial predicate, so PostgreSQL can infer the unique index.
const enqueueOnceSQL = `
INSERT INTO jobs (tenant_id, kind, payload, trace_parent, status, run_at, max_attempts, idempotency_key, created_at, updated_at)
VALUES ($1, $2, $3::jsonb, NULLIF($4, ''), 'queued', $5, $6, $7, now(), now())
ON CONFLICT (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL
DO NOTHING
RETURNING id`

const existingIdempotentJobSQL = `
SELECT id FROM jobs WHERE tenant_id = $1 AND idempotency_key = $2`

// queryRower is implemented by both *pgxpool.Pool and pgx.Tx.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Queue enqueues jobs for a Worker to process.
type Queue struct {
	pool *pgxpool.Pool
}

// NewQueue returns a queue backed by pool.
func NewQueue(pool *pgxpool.Pool) *Queue {
	return &Queue{pool: pool}
}

// Enqueue inserts job as queued work and returns its id. A zero RunAt means
// now, a zero MaxAttempts means DefaultMaxAttempts, and a nil Payload means an
// empty JSON object. A zero TenantID is resolved from the context, falling back
// to tenant.DefaultID.
func (q *Queue) Enqueue(ctx context.Context, job Job) (int64, error) {
	normalized, err := prepareJob(ctx, job, time.Now())
	if err != nil {
		return 0, err
	}
	if q == nil || q.pool == nil {
		return 0, ErrNoPool
	}
	return insertJob(ctx, q.pool, normalized)
}

// EnqueueTx inserts job using tx, so the job is committed atomically with the
// caller's transaction. Tenant resolution matches Enqueue.
func (q *Queue) EnqueueTx(ctx context.Context, tx pgx.Tx, job Job) (int64, error) {
	normalized, err := prepareJob(ctx, job, time.Now())
	if err != nil {
		return 0, err
	}
	if tx == nil {
		return 0, ErrNoTx
	}
	return insertJob(ctx, tx, normalized)
}

// EnqueueOnce inserts job unless a job with the same tenant and IdempotencyKey
// already exists. It returns the existing or newly inserted id and whether a
// row was inserted. A non-empty IdempotencyKey is required; duplicate
// concurrent producers safely converge on one row.
func (q *Queue) EnqueueOnce(ctx context.Context, job Job) (id int64, created bool, err error) {
	normalized, err := prepareIdempotentJob(ctx, job, time.Now())
	if err != nil {
		return 0, false, err
	}
	if q == nil || q.pool == nil {
		return 0, false, ErrNoPool
	}
	return insertJobOnce(ctx, q.pool, normalized)
}

// EnqueueOnceTx is EnqueueOnce using tx, so the deduplicated job commits
// atomically with the caller's transaction.
func (q *Queue) EnqueueOnceTx(ctx context.Context, tx pgx.Tx, job Job) (id int64, created bool, err error) {
	normalized, err := prepareIdempotentJob(ctx, job, time.Now())
	if err != nil {
		return 0, false, err
	}
	if tx == nil {
		return 0, false, ErrNoTx
	}
	return insertJobOnce(ctx, tx, normalized)
}

func prepareIdempotentJob(ctx context.Context, job Job, now time.Time) (Job, error) {
	if strings.TrimSpace(job.IdempotencyKey) == "" {
		return Job{}, ErrEmptyIdempotencyKey
	}
	return prepareJob(ctx, job, now)
}

// prepareJob resolves the tenant before applying the usual defaults. The
// context is authoritative when the caller did not set a tenant explicitly, so
// request-scoped producers inherit tenant isolation without extra plumbing.
func prepareJob(ctx context.Context, job Job, now time.Time) (Job, error) {
	if job.TenantID == 0 {
		job.TenantID = tenant.EffectiveID(ctx)
	}
	if job.TenantID <= 0 {
		return Job{}, ErrInvalidTenant
	}
	return job.normalize(now)
}

func insertJob(ctx context.Context, db queryRower, job Job) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, insertJobSQL,
		job.TenantID,
		job.Kind,
		string(job.Payload),
		tracing.CaptureRemoteContext(ctx),
		job.RunAt,
		job.MaxAttempts,
		nullableIdempotencyKey(job.IdempotencyKey),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue %q: %w", job.Kind, err)
	}
	return id, nil
}

func insertJobOnce(ctx context.Context, db queryRower, job Job) (id int64, created bool, err error) {
	err = db.QueryRow(ctx, enqueueOnceSQL,
		job.TenantID,
		job.Kind,
		string(job.Payload),
		tracing.CaptureRemoteContext(ctx),
		job.RunAt,
		job.MaxAttempts,
		job.IdempotencyKey,
	).Scan(&id)
	switch {
	case err == nil:
		return id, true, nil
	case errors.Is(err, pgx.ErrNoRows):
		// The conflict target matched an existing row. Reading it back gives
		// callers a stable id even though this call inserted nothing.
		if err := db.QueryRow(ctx, existingIdempotentJobSQL, job.TenantID, job.IdempotencyKey).Scan(&id); err != nil {
			return 0, false, fmt.Errorf("jobs: read idempotent job %q: %w", job.Kind, err)
		}
		return id, false, nil
	default:
		return 0, false, fmt.Errorf("jobs: enqueue once %q: %w", job.Kind, err)
	}
}

func nullableIdempotencyKey(key string) any {
	if key == "" {
		return nil
	}
	return key
}

// PurgeTerminal deletes succeeded and dead jobs whose last update is older than
// olderThan and returns the number of deleted rows. Workers run retention
// automatically, so this is for manual operational cleanup; it records the
// deleted rows in JobsPurgedTotal just like the worker does. A non-positive
// window returns ErrInvalidRetention and a nil pool returns ErrNoPool. If a
// later batch fails, the rows deleted before the error are counted and returned
// with the error. Queued and running jobs are never deleted.
func (q *Queue) PurgeTerminal(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, ErrInvalidRetention
	}
	if q == nil || q.pool == nil {
		return 0, ErrNoPool
	}
	purged, err := newPGStore(q.pool).PurgeTerminal(ctx, olderThan)
	if purged > 0 {
		metrics.JobsPurgedTotal.Add(float64(purged))
	}
	return purged, err
}
