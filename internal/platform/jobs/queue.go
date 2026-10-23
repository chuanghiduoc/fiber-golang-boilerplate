package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertJobSQL inserts one queued job and returns its id. The cast makes the
// parameter type explicit so a JSON document is never mistaken for bytea.
const insertJobSQL = `
INSERT INTO jobs (kind, payload, status, run_at, max_attempts, created_at, updated_at)
VALUES ($1, $2::jsonb, 'queued', $3, $4, now(), now())
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
// empty JSON object.
func (q *Queue) Enqueue(ctx context.Context, job Job) (int64, error) {
	normalized, err := job.normalize(time.Now())
	if err != nil {
		return 0, err
	}
	if q == nil || q.pool == nil {
		return 0, ErrNoPool
	}
	return insertJob(ctx, q.pool, normalized)
}

// EnqueueTx inserts job using tx, so the job is committed atomically with the
// caller's transaction.
func (q *Queue) EnqueueTx(ctx context.Context, tx pgx.Tx, job Job) (int64, error) {
	normalized, err := job.normalize(time.Now())
	if err != nil {
		return 0, err
	}
	if tx == nil {
		return 0, ErrNoTx
	}
	return insertJob(ctx, tx, normalized)
}

func insertJob(ctx context.Context, db queryRower, job Job) (int64, error) {
	var id int64
	err := db.QueryRow(ctx, insertJobSQL,
		job.Kind,
		string(job.Payload),
		job.RunAt,
		job.MaxAttempts,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("jobs: enqueue %q: %w", job.Kind, err)
	}
	return id, nil
}
