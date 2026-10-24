package jobs

import (
	"context"
	"fmt"
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
INSERT INTO jobs (tenant_id, kind, payload, trace_parent, status, run_at, max_attempts, created_at, updated_at)
VALUES ($1, $2, $3::jsonb, NULLIF($4, ''), 'queued', $5, $6, now(), now())
RETURNING id`

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
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue %q: %w", job.Kind, err)
	}
	return id, nil
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
