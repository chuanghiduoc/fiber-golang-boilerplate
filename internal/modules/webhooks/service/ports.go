package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const (
	// StatusActive enables an endpoint or inbound source.
	StatusActive = "active"
	// StatusDisabled pauses deliveries/source ingestion without deleting data.
	StatusDisabled = "disabled"

	// DeliveryPending is queued or between attempts.
	DeliveryPending = "pending"
	// DeliverySucceeded means a 2xx response was received.
	DeliverySucceeded = "succeeded"
	// DeliveryDead is terminal after permanent failure or exhausted retries.
	DeliveryDead = "dead"

	// DeliveryJobKind is the durable job kind for one delivery attempt.
	DeliveryJobKind = "webhook.delivery"

	// SecretRotationGrace is how long a retired endpoint secret keeps
	// co-signing deliveries after a rotation, and how long the inbound
	// receiver accepts a source's previous secret. Receivers and senders get
	// 24 hours to switch to the new secret.
	SecretRotationGrace = 24 * time.Hour
)

var (
	// ErrNotClaimable means the delivery is terminal or another attempt holds
	// the claim lease. Workers reload it to decide whether to retry the job.
	ErrNotClaimable = errors.New("webhooks: delivery is not claimable")
	// ErrDeliveryLeaseLost means an outcome write matched no row: the delivery
	// is terminal or another worker claimed a newer attempt after this
	// worker's claim lease expired. The stale result must be discarded.
	ErrDeliveryLeaseLost = errors.New("webhooks: delivery claim lease lost")
)

// Endpoint is an outbound endpoint and its subscribed event-type patterns.
type Endpoint struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	URL         string
	Description string
	EventTypes  []string
	Status      string
	MaxAttempts int32
	TimeoutMS   int32
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// Delivery is one delivery row plus the event envelope it delivers. EventType
// and OccurredAt are always set; DataVersion, the actor, and Payload are set
// only by detail reads.
type Delivery struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	EndpointID     uuid.UUID
	EventID        uuid.UUID
	SecretID       uuid.UUID
	URL            string
	Status         string
	Attempts       int32
	MaxAttempts    int32
	TimeoutMS      int32
	NextAttemptAt  time.Time
	LastStatusCode *int
	LastError      string
	DeliveredAt    *time.Time
	ReplayedFromID *uuid.UUID
	CreatedAt      time.Time
	UpdatedAt      time.Time

	EventType   string
	DataVersion int32
	ActorType   string
	ActorID     string
	OccurredAt  time.Time
	Payload     json.RawMessage
}

// DeliveryAttempt records one HTTP attempt.
type DeliveryAttempt struct {
	ID         uuid.UUID
	DeliveryID uuid.UUID
	Attempt    int32
	StatusCode *int
	DurationMS int32
	Error      string
	CreatedAt  time.Time
}

// InboundSource is a tenant-owned HMAC-authenticated receiver. Its current and
// immediately previous sealed secrets live in the versioned
// webhook_inbound_source_secrets table.
type InboundSource struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Name      string
	Slug      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// InboundEvent is one authenticated, deduplicated inbound event.
type InboundEvent struct {
	ID              uuid.UUID
	TenantID        uuid.UUID
	SourceID        uuid.UUID
	ExternalEventID string
	EventType       string
	Payload         json.RawMessage
	ReceivedAt      time.Time
}

// InboundReceipt acknowledges a received inbound event.
type InboundReceipt struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	SourceID   uuid.UUID
	EventID    string
	EventType  string
	ReceivedAt time.Time
	Duplicate  bool
}

// ActiveEndpoint is the snapshot needed to fan an event out to one endpoint.
// SecretID names the exact secret version the delivery must snapshot.
type ActiveEndpoint struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	URL         string
	EventTypes  []string
	MaxAttempts int32
	TimeoutMS   int32
	SecretID    uuid.UUID
}

// SigningSecrets is an endpoint's (or inbound source's) active sealed secret
// plus the immediately previous sealed secret while its explicit rotation
// grace window is open. PreviousCiphertext is empty once the grace expires.
type SigningSecrets struct {
	CurrentCiphertext  string
	PreviousCiphertext string
}

// CreateEndpointInput is the endpoint configuration a caller may set.
type CreateEndpointInput struct {
	URL         string
	Description string
	EventTypes  []string
	Status      string
	MaxAttempts *int
	TimeoutMS   *int
}

// UpdateEndpointInput replaces the mutable endpoint configuration.
type UpdateEndpointInput struct {
	URL         string
	Description string
	EventTypes  []string
	Status      string
	MaxAttempts *int
	TimeoutMS   *int
	// ExpectedUpdatedAt is the ETag precondition; nil is unconditional.
	ExpectedUpdatedAt *time.Time
}

// CreateEndpointDraft is a newly created endpoint configuration. SealSecret
// runs inside the create transaction with the row's generated id, so the
// secret's AAD is bound to the endpoint that owns it.
type CreateEndpointDraft struct {
	URL         string
	Description string
	EventTypes  []string
	Status      string
	MaxAttempts int32
	TimeoutMS   int32
	SealSecret  func(endpointID uuid.UUID) (string, error)
}

// UpdateEndpointDraft replaces the mutable endpoint configuration.
type UpdateEndpointDraft struct {
	ID          uuid.UUID
	URL         string
	Description string
	EventTypes  []string
	Status      string
	MaxAttempts int32
	TimeoutMS   int32
	// ExpectedUpdatedAt is the ETag precondition; nil is unconditional.
	ExpectedUpdatedAt *time.Time
}

// DeliveryDraft creates one delivery row and its first durable job atomically.
// EventType, DataVersion, ActorType/ActorID, SubjectUserIDs, EventCreatedAt,
// and Payload (the envelope data) describe the event: they are written to
// webhook_events by the first delivery of EventID and ignored afterwards.
type DeliveryDraft struct {
	TenantID       uuid.UUID
	EndpointID     uuid.UUID
	EventID        uuid.UUID
	SecretID       uuid.UUID
	EventType      string
	DataVersion    int32
	ActorType      string
	ActorID        string
	SubjectUserIDs []uuid.UUID
	EventCreatedAt time.Time
	URL            string
	Payload        json.RawMessage
	MaxAttempts    int32
	TimeoutMS      int32
	RunAt          time.Time
	ReplayedFromID *uuid.UUID
	// IsReplay inserts a new delivery for an existing event instead of taking
	// part in the per-endpoint dispatch deduplication.
	IsReplay bool
}

// ListDeliveriesParams filters the tenant's delivery history. A zero
// EndpointID means every endpoint.
type ListDeliveriesParams struct {
	EndpointID uuid.UUID
	Status     string
	Cursor     pagination.DBCursor
}

// RetryDraft reschedules a pending delivery and enqueues its next job in one
// transaction.
type RetryDraft struct {
	DeliveryID uuid.UUID
	// Attempt is the claimed attempt number; it fences the write.
	Attempt       int32
	NextAttemptAt time.Time
	StatusCode    int
	Error         string
}

// AttemptDraft records one HTTP attempt.
type AttemptDraft struct {
	DeliveryID uuid.UUID
	Attempt    int32
	StatusCode int
	Duration   time.Duration
	Error      string
}

// CreateInboundSourceInput is the source configuration a caller may set.
type CreateInboundSourceInput struct {
	Name   string
	Slug   string
	Status string
}

// UpdateInboundSourceInput replaces the mutable source configuration.
type UpdateInboundSourceInput struct {
	Name   string
	Slug   string
	Status string
	// ExpectedUpdatedAt is the ETag precondition; nil is unconditional.
	ExpectedUpdatedAt *time.Time
}

// InboundSourceDraft creates a tenant-owned inbound source. SealSecret runs
// inside the create transaction with the row's generated id.
type InboundSourceDraft struct {
	Name       string
	Slug       string
	Status     string
	SealSecret func(sourceID uuid.UUID) (string, error)
}

// UpdateInboundSourceDraft replaces the mutable source configuration.
type UpdateInboundSourceDraft struct {
	ID     uuid.UUID
	Name   string
	Slug   string
	Status string
	// ExpectedUpdatedAt is the ETag precondition; nil is unconditional.
	ExpectedUpdatedAt *time.Time
}

// InboundEventDraft is one authenticated, deduplicated inbound event.
type InboundEventDraft struct {
	SourceID        uuid.UUID
	ExternalEventID string
	EventType       string
	Signature       string
	Payload         json.RawMessage
}

// DeliveryJobPayload is the job payload for one delivery attempt.
type DeliveryJobPayload struct {
	DeliveryID uuid.UUID `json:"deliveryId"`
}

// EndpointStore covers endpoint CRUD and secret snapshots.
type EndpointStore interface {
	CreateEndpoint(ctx context.Context, draft CreateEndpointDraft) (*Endpoint, error)
	GetEndpoint(ctx context.Context, id uuid.UUID) (*Endpoint, error)
	ListEndpoints(ctx context.Context, cur pagination.DBCursor) ([]Endpoint, error)
	UpdateEndpoint(ctx context.Context, draft UpdateEndpointDraft) (*Endpoint, error)
	DeleteEndpoint(ctx context.Context, id uuid.UUID) error
	RotateEndpointSecret(ctx context.Context, id uuid.UUID, ciphertext string, expirePrevious bool) (*Endpoint, error)
	ActiveEndpoints(ctx context.Context, tenantID uuid.UUID) ([]ActiveEndpoint, error)
	ActiveSecretID(ctx context.Context, endpointID uuid.UUID) (uuid.UUID, error)
	SigningSecrets(ctx context.Context, endpointID uuid.UUID) (*SigningSecrets, error)
}

// DeliveryStore covers the outbox, attempts, claims, and retention.
type DeliveryStore interface {
	CreateDeliveryWithJob(ctx context.Context, draft DeliveryDraft) (deliveryID uuid.UUID, created bool, err error)
	GetDelivery(ctx context.Context, id uuid.UUID) (*Delivery, error)
	ListDeliveries(ctx context.Context, params ListDeliveriesParams) ([]Delivery, error)
	ClaimDelivery(ctx context.Context, id uuid.UUID, lease time.Duration) (*Delivery, error)
	MarkDeliverySucceeded(ctx context.Context, id uuid.UUID, attempt int32, statusCode int) error
	MarkDeliveryDead(ctx context.Context, id uuid.UUID, attempt int32, statusCode int, reason string) error
	ReleaseDeliveryClaim(ctx context.Context, id uuid.UUID, attempt int32) error
	ExpireUnclaimableDelivery(ctx context.Context, id uuid.UUID) (bool, error)
	RescheduleDeliveryWithJob(ctx context.Context, draft RetryDraft) error
	CreateDeliveryAttempt(ctx context.Context, draft AttemptDraft) error
	ListDeliveryAttempts(ctx context.Context, deliveryID uuid.UUID) ([]DeliveryAttempt, error)
	PurgeDeliveries(ctx context.Context, retentionDays int32) (int64, error)
	PurgeOrphanEvents(ctx context.Context, retentionDays int32) (int64, error)
}

// InboundStore covers inbound source CRUD, secrets, and deduplicated events.
type InboundStore interface {
	CreateInboundSource(ctx context.Context, draft InboundSourceDraft) (*InboundSource, error)
	GetInboundSource(ctx context.Context, id uuid.UUID) (*InboundSource, error)
	GetInboundSourceByID(ctx context.Context, id uuid.UUID) (*InboundSource, error)
	ListInboundSources(ctx context.Context, cur pagination.DBCursor) ([]InboundSource, error)
	UpdateInboundSource(ctx context.Context, draft UpdateInboundSourceDraft) (*InboundSource, error)
	DeleteInboundSource(ctx context.Context, id uuid.UUID) (*InboundSource, error)
	RotateInboundSourceSecret(ctx context.Context, id uuid.UUID, ciphertext string, expirePrevious bool) (*InboundSource, error)
	InboundSigningSecrets(ctx context.Context, sourceID uuid.UUID) (*SigningSecrets, error)
	CreateInboundEvent(ctx context.Context, draft InboundEventDraft) (eventID uuid.UUID, created bool, err error)
	GetInboundEventByExternalID(ctx context.Context, sourceID uuid.UUID, externalEventID string) (*InboundEvent, error)
	ListInboundEvents(ctx context.Context, sourceID uuid.UUID, cur pagination.DBCursor) ([]InboundEvent, error)
	PurgeInboundEvents(ctx context.Context, retentionDays int32) (int64, error)
}

// DispatchStore is the narrow surface the outbound dispatcher needs.
type DispatchStore interface {
	ActiveEndpoints(ctx context.Context, tenantID uuid.UUID) ([]ActiveEndpoint, error)
	CreateDeliveryWithJob(ctx context.Context, draft DeliveryDraft) (deliveryID uuid.UUID, created bool, err error)
}
