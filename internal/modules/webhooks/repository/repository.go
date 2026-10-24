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
	"github.com/jackc/pgx/v5/pgxpool"

	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const (
	// DeliveryJobKind is the durable job kind for one delivery attempt.
	DeliveryJobKind = "webhook.delivery"
	// deliveryJobMaxAttempts bounds retries of the infrastructure job itself.
	// Delivery-level retries are separate jobs scheduled by the worker. The
	// budget is generous because a transient database outage between an HTTP
	// attempt and bookkeeping should not strand the delivery.
	deliveryJobMaxAttempts = 20
)

var (
	// ErrNoPool is returned when the repository has no database pool.
	ErrNoPool = errors.New("webhooks: database pool is nil")
	// ErrNoQueue is returned when a transactional delivery/job write has no
	// durable queue.
	ErrNoQueue = errors.New("webhooks: job queue is nil")
	// ErrNotClaimable means the delivery is terminal or another attempt holds
	// the claim lease. Workers reload it to decide whether to retry the job.
	ErrNotClaimable = errors.New("webhooks: delivery is not claimable")
)

// DeliveryJobPayload is the job payload for one delivery attempt.
type DeliveryJobPayload struct {
	DeliveryID int64 `json:"deliveryId"`
}

// ActiveEndpoint is the snapshot needed to fan an event out to one endpoint.
type ActiveEndpoint struct {
	ID               int64
	TenantID         int64
	URL              string
	EventTypes       []string
	MaxAttempts      int32
	TimeoutMS        int32
	SecretCiphertext string
}

// CreateEndpointDraft is a newly created endpoint plus its first sealed secret.
type CreateEndpointDraft struct {
	URL              string
	Description      string
	EventTypes       []string
	Status           string
	MaxAttempts      int32
	TimeoutMS        int32
	SecretCiphertext string
}

// UpdateEndpointDraft replaces the mutable endpoint configuration.
type UpdateEndpointDraft struct {
	ID          int64
	URL         string
	Description string
	EventTypes  []string
	Status      string
	MaxAttempts int32
	TimeoutMS   int32
}

// DeliveryDraft creates one delivery row and its first durable job atomically.
type DeliveryDraft struct {
	TenantID         int64
	EndpointID       int64
	EventID          string
	EventType        string
	EventCreatedAt   time.Time
	URL              string
	Payload          json.RawMessage
	SecretCiphertext string
	MaxAttempts      int32
	RunAt            time.Time
	ReplayedFromID   int64
}

// ListDeliveriesParams filters the tenant's delivery history.
type ListDeliveriesParams struct {
	EndpointID int64
	Status     string
	Cursor     pagination.DBCursor
}

// RetryDraft reschedules a pending delivery and enqueues its next job in one
// transaction.
type RetryDraft struct {
	DeliveryID    int64
	NextAttemptAt time.Time
	StatusCode    int
	Error         string
}

// AttemptDraft records one HTTP attempt.
type AttemptDraft struct {
	DeliveryID int64
	Attempt    int32
	StatusCode int
	Duration   time.Duration
	Error      string
}

// InboundSourceDraft creates a tenant-owned inbound source.
type InboundSourceDraft struct {
	Name             string
	Slug             string
	SecretCiphertext string
}

// UpdateInboundSourceDraft replaces the mutable source configuration.
type UpdateInboundSourceDraft struct {
	ID     int64
	Name   string
	Slug   string
	Status string
}

// InboundEventDraft is one authenticated, deduplicated inbound event.
type InboundEventDraft struct {
	SourceID        int64
	ExternalEventID string
	EventType       string
	Signature       string
	Payload         json.RawMessage
}

// EndpointStore covers endpoint CRUD and secret snapshots.
type EndpointStore interface {
	CreateEndpoint(ctx context.Context, draft CreateEndpointDraft) (*webhooksqlc.WebhookEndpoint, error)
	GetEndpoint(ctx context.Context, id int64) (*webhooksqlc.WebhookEndpoint, error)
	ListEndpoints(ctx context.Context, cur pagination.DBCursor) ([]webhooksqlc.WebhookEndpoint, error)
	UpdateEndpoint(ctx context.Context, draft UpdateEndpointDraft) (*webhooksqlc.WebhookEndpoint, error)
	DeleteEndpoint(ctx context.Context, id int64) (*webhooksqlc.WebhookEndpoint, error)
	RotateEndpointSecret(ctx context.Context, id int64, ciphertext string) (*webhooksqlc.WebhookEndpoint, error)
	ActiveEndpoints(ctx context.Context, tenantID int64) ([]ActiveEndpoint, error)
	ActiveEndpointSecret(ctx context.Context, endpointID int64) (string, error)
}

// DeliveryStore covers the outbox, attempts, claims, and retention.
type DeliveryStore interface {
	CreateDeliveryWithJob(ctx context.Context, draft DeliveryDraft) (deliveryID int64, created bool, err error)
	GetDelivery(ctx context.Context, id int64) (*webhooksqlc.WebhookDelivery, error)
	ListDeliveries(ctx context.Context, params ListDeliveriesParams) ([]webhooksqlc.WebhookDelivery, error)
	ClaimDelivery(ctx context.Context, id int64, lease time.Duration) (*webhooksqlc.WebhookDelivery, error)
	MarkDeliverySucceeded(ctx context.Context, id int64, statusCode int) error
	MarkDeliveryDead(ctx context.Context, id int64, statusCode int, reason string) error
	RescheduleDeliveryWithJob(ctx context.Context, draft RetryDraft) error
	CreateDeliveryAttempt(ctx context.Context, draft AttemptDraft) error
	ListDeliveryAttempts(ctx context.Context, deliveryID int64) ([]webhooksqlc.WebhookDeliveryAttempt, error)
	PurgeDeliveries(ctx context.Context, retentionDays int32) (int64, error)
}

// InboundStore covers inbound source CRUD and deduplicated events.
type InboundStore interface {
	CreateInboundSource(ctx context.Context, draft InboundSourceDraft) (*webhooksqlc.WebhookInboundSource, error)
	GetInboundSource(ctx context.Context, id int64) (*webhooksqlc.WebhookInboundSource, error)
	GetInboundSourceByID(ctx context.Context, id int64) (*webhooksqlc.WebhookInboundSource, error)
	ListInboundSources(ctx context.Context, cur pagination.DBCursor) ([]webhooksqlc.WebhookInboundSource, error)
	UpdateInboundSource(ctx context.Context, draft UpdateInboundSourceDraft) (*webhooksqlc.WebhookInboundSource, error)
	DeleteInboundSource(ctx context.Context, id int64) (*webhooksqlc.WebhookInboundSource, error)
	RotateInboundSourceSecret(ctx context.Context, id int64, ciphertext string) (*webhooksqlc.WebhookInboundSource, error)
	CreateInboundEvent(ctx context.Context, draft InboundEventDraft) (eventID int64, created bool, err error)
	GetInboundEventByExternalID(ctx context.Context, sourceID int64, externalEventID string) (*webhooksqlc.WebhookInboundEvent, error)
	ListInboundEvents(ctx context.Context, sourceID int64, cur pagination.DBCursor) ([]webhooksqlc.WebhookInboundEvent, error)
	PurgeInboundEvents(ctx context.Context, retentionDays int32) (int64, error)
}

// DispatchStore is the narrow surface the outbound dispatcher needs.
type DispatchStore interface {
	ActiveEndpoints(ctx context.Context, tenantID int64) ([]ActiveEndpoint, error)
	CreateDeliveryWithJob(ctx context.Context, draft DeliveryDraft) (deliveryID int64, created bool, err error)
}

// JobQueue is implemented by platform/jobs.Queue. The narrow interface keeps
// repository tests independent of a live queue while preserving atomic
// delivery+job commits.
type JobQueue interface {
	EnqueueTx(ctx context.Context, tx pgx.Tx, job jobs.Job) (int64, error)
}

// Postgres implements every webhooks persistence interface.
type Postgres struct {
	pool  *pgxpool.Pool
	q     *webhooksqlc.Queries
	queue JobQueue
}

var (
	_ EndpointStore = (*Postgres)(nil)
	_ DeliveryStore = (*Postgres)(nil)
	_ InboundStore  = (*Postgres)(nil)
	_ DispatchStore = (*Postgres)(nil)
)

// New returns a repository backed by pool. queue may be nil only for reads and
// non-job writes; delivery creation and retry scheduling fail with ErrNoQueue.
func New(pool *pgxpool.Pool, queue JobQueue) *Postgres {
	return &Postgres{pool: pool, q: webhooksqlc.New(pool), queue: queue}
}

func (r *Postgres) withTx(ctx context.Context, fn func(tx pgx.Tx, q *webhooksqlc.Queries) error) (err error) {
	if r == nil || r.pool == nil {
		return ErrNoPool
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("webhooks: begin transaction: %w", err)
	}
	defer func() {
		rbErr := tx.Rollback(context.WithoutCancel(ctx))
		if rbErr == nil || errors.Is(rbErr, pgx.ErrTxClosed) {
			return
		}
		if err != nil {
			err = errors.Join(err, fmt.Errorf("webhooks: rollback: %w", rbErr))
			return
		}
		err = fmt.Errorf("webhooks: rollback: %w", rbErr)
	}()

	if err := fn(tx, r.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("webhooks: commit: %w", err)
	}
	return nil
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

func jobPayload(deliveryID int64) json.RawMessage {
	payload, _ := json.Marshal(DeliveryJobPayload{DeliveryID: deliveryID})
	return payload
}

func enqueueDeliveryJob(ctx context.Context, tx pgx.Tx, queue JobQueue, tenantID, deliveryID int64, runAt time.Time) error {
	if queue == nil {
		return ErrNoQueue
	}
	_, err := queue.EnqueueTx(ctx, tx, jobs.Job{
		TenantID:    tenantID,
		Kind:        DeliveryJobKind,
		Payload:     jobPayload(deliveryID),
		RunAt:       runAt,
		MaxAttempts: deliveryJobMaxAttempts,
	})
	if err != nil {
		return fmt.Errorf("webhooks: enqueue delivery job: %w", err)
	}
	return nil
}
