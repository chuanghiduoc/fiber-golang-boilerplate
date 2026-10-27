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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	webhooksvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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
	EnqueueTx(ctx context.Context, tx pgx.Tx, job jobs.Job) (uuid.UUID, error)
}

// Postgres implements every webhooks persistence interface.
type Postgres struct {
	db    database.DBTX
	tx    *database.TxManager
	q     *webhooksqlc.Queries
	queue JobQueue
	audit audit.TxRecorder
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
func New(db database.DBTX, tx *database.TxManager, queue JobQueue, recorders ...audit.TxRecorder) *Postgres {
	var recorder audit.TxRecorder
	if len(recorders) > 0 {
		recorder = recorders[0]
	}
	return &Postgres{db: db, tx: tx, q: webhooksqlc.New(db), queue: queue, audit: recorder}
}

func (r *Postgres) withTx(ctx context.Context, fn func(tx pgx.Tx, q *webhooksqlc.Queries) error) error {
	if r == nil || r.tx == nil {
		return ErrNoPool
	}
	return r.tx.WithTx(ctx, func(tx pgx.Tx) error {
		return fn(tx, r.q.WithTx(tx))
	})
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

func nullableUUID(value *uuid.UUID) uuid.NullUUID {
	if value == nil || *value == uuid.Nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *value, Valid: true}
}

// parseActorID converts the event envelope's string actor id into the nullable
// UUID webhook_events stores. A system actor has no id; any other actor must
// name a valid UUID.
func parseActorID(value string) (uuid.NullUUID, error) {
	if value == "" {
		return uuid.NullUUID{}, nil
	}
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return uuid.NullUUID{}, fmt.Errorf("webhooks: invalid actor id %q", value)
	}
	return uuid.NullUUID{UUID: id, Valid: true}, nil
}

// uuidString renders a nullable UUID for the event envelope, where a system
// actor has an empty id.
func uuidString(value uuid.NullUUID) string {
	if !value.Valid {
		return ""
	}
	return value.UUID.String()
}

func intPtr(value pgtype.Int4) *int {
	if !value.Valid {
		return nil
	}
	v := int(value.Int32)
	return &v
}

func uuidPtr(value uuid.NullUUID) *uuid.UUID {
	if !value.Valid {
		return nil
	}
	v := value.UUID
	return &v
}

func jobPayload(deliveryID uuid.UUID) json.RawMessage {
	payload, _ := json.Marshal(webhooksvc.DeliveryJobPayload{DeliveryID: deliveryID})
	return payload
}

func enqueueDeliveryJob(ctx context.Context, tx pgx.Tx, queue JobQueue, tenantID, deliveryID uuid.UUID, runAt time.Time) error {
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

// mapConditionalUpdate turns a conditional UPDATE's no-row result into 412
// when the row still exists, or 404 when it was deleted concurrently. A
// missing precondition is the caller's concern; this helper is only reached
// from the conditional branch.
func mapConditionalUpdate(
	_ context.Context,
	_ *webhooksqlc.Queries,
	err error,
	exists func() error,
) error {
	if !errors.Is(err, pgx.ErrNoRows) {
		return mapNotFound(err)
	}
	switch existsErr := exists(); {
	case existsErr == nil:
		return apperror.NewPreconditionFailed("the resource has changed since it was read; refresh and retry")
	case errors.Is(existsErr, pgx.ErrNoRows):
		return apperror.ErrNotFound
	default:
		return existsErr
	}
}

// timestamptzValue wraps a non-zero time for a conditional statement.
func timestamptzValue(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
