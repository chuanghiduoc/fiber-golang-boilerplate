// Package repository is the webhooks module's persistence layer. Every
// tenant-scoped query derives the tenant from the request context, except the
// public inbound receiver's deliberate global source lookup by ID.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	webhooksvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const (
	// deliveryJobMaxAttempts bounds retries of the infrastructure job itself.
	// Delivery-level retries are separate jobs scheduled by the worker. The
	// budget is generous because a transient database outage between an HTTP
	// attempt and bookkeeping should not strand the delivery.
	deliveryJobMaxAttempts = 20
	// purgeBatchSize bounds one retention DELETE statement.
	purgeBatchSize = 1000
)

var (
	// ErrNoPool is returned when the repository has no database pool.
	ErrNoPool = errors.New("webhooks: database pool is nil")
	// ErrNoQueue is returned when a transactional delivery/job write has no
	// durable queue.
	ErrNoQueue = errors.New("webhooks: job queue is nil")
)

// JobQueue is implemented by platform/jobs.Queue. The narrow interface keeps
// repository tests independent of a live queue while preserving atomic
// delivery+job commits.
type JobQueue interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, job jobs.Job) (int64, error)
}

// Postgres implements every webhooks persistence interface.
type Postgres struct {
	db    database.DBTX
	tx    *database.TxManager
	q     *webhooksqlc.Queries
	queue JobQueue
}

var (
	_ webhooksvc.EndpointStore = (*Postgres)(nil)
	_ webhooksvc.DeliveryStore = (*Postgres)(nil)
	_ webhooksvc.InboundStore  = (*Postgres)(nil)
	_ webhooksvc.DispatchStore = (*Postgres)(nil)
)

// New returns a repository backed by db for single statements and tx for
// multi-statement work. queue may be nil only for reads and non-job writes;
// delivery creation and retry scheduling fail with ErrNoQueue.
func New(db database.DBTX, tx *database.TxManager, queue JobQueue) *Postgres {
	return &Postgres{db: db, tx: tx, q: webhooksqlc.New(db), queue: queue}
}

func (r *Postgres) withTx(ctx context.Context, fn func(tx pgx.Tx, q *webhooksqlc.Queries) error) error {
	if r == nil || r.tx == nil {
		return ErrNoPool
	}
	return r.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return fn(tx, r.q.WithTx(tx))
	})
}

// tenantID returns the tenant every scoped query must use.
func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	return err
}

func mapInboundConflict(err error) error {
	if database.IsUniqueViolation(err) {
		return apperror.NewConflict("inbound source slug already exists")
	}
	return err
}

func timestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}

func nullableInt8(value int64) pgtype.Int8 {
	if value == 0 {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: value, Valid: true}
}

func timePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time
	return &t
}

func intPtr(value pgtype.Int4) *int {
	if !value.Valid {
		return nil
	}
	v := int(value.Int32)
	return &v
}

func int64Ptr(value pgtype.Int8) *int64 {
	if !value.Valid {
		return nil
	}
	v := value.Int64
	return &v
}

func jobPayload(deliveryID int64) json.RawMessage {
	payload, _ := json.Marshal(webhooksvc.DeliveryJobPayload{DeliveryID: deliveryID})
	return payload
}

func enqueueDeliveryJob(ctx context.Context, tx pgx.Tx, queue JobQueue, tenantID, deliveryID int64, runAt time.Time) error {
	if queue == nil {
		return ErrNoQueue
	}
	_, err := queue.EnqueueTx(ctx, tx, jobs.Job{
		TenantID:    tenantID,
		Kind:        webhooksvc.DeliveryJobKind,
		Payload:     jobPayload(deliveryID),
		RunAt:       runAt,
		MaxAttempts: deliveryJobMaxAttempts,
	})
	if err != nil {
		return fmt.Errorf("webhooks: enqueue delivery job: %w", err)
	}
	return nil
}
