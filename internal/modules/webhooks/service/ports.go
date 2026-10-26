package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

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
	ID          int64
	TenantID    int64
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
	ID               int64
	TenantID         int64
	EndpointID       int64
	EventID          string
	URL              string
	SecretCiphertext string
	Status           string
	Attempts         int32
	MaxAttempts      int32
	TimeoutMS        int32
	NextAttemptAt    time.Time
	LastStatusCode   *int
	LastError        string
	DeliveredAt      *time.Time
	ReplayedFromID   *int64
	CreatedAt        time.Time
	UpdatedAt        time.Time

	EventType   string
	DataVersion int32
	ActorType   string
	ActorID     string
	OccurredAt  time.Time
	Payload     json.RawMessage
}

// DeliveryAttempt records one HTTP attempt.
type DeliveryAttempt struct {
	ID         int64
	DeliveryID int64
	Attempt    int32
	StatusCode *int
	DurationMS int32
	Error      string
	CreatedAt  time.Time
}

// InboundSource is a tenant-owned HMAC-authenticated receiver. The previous
// sealed secret is kept, with an explicit expiry, for the rotation grace
// window so a sender that has not switched yet still verifies.
type InboundSource struct {
	ID                       int64
	TenantID                 int64
	Name                     string
	Slug                     string
	SecretCiphertext         string
	PreviousSecretCiphertext string
	PreviousSecretExpiresAt  *time.Time
	Status                   string
	CreatedAt                time.Time
	UpdatedAt                time.Time
	DeletedAt                *time.Time
}

// InboundEvent is one authenticated, deduplicated inbound event.
type InboundEvent struct {
	ID              int64
	TenantID        int64
	SourceID        int64
	ExternalEventID string
	EventType       string
	Payload         json.RawMessage
	ReceivedAt      time.Time
}

// InboundReceipt acknowledges a received inbound event.
type InboundReceipt struct {
	ID         int64
	TenantID   int64
	SourceID   int64
	EventID    string
	EventType  string
	ReceivedAt time.Time
	Duplicate  bool
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

// SigningSecrets is an endpoint's active sealed secret plus the immediately
// previous sealed secret while its explicit rotation grace window is open.
// PreviousCiphertext is empty once the grace expires.
type SigningSecrets struct {
	CurrentCiphertext  string
	PreviousCiphertext string
}

// CreateEndpointInput is the endpoint configuration a caller may set.
type CreateEndpointInput struct {
	URL         string
	Description string
	EventTypes  []string
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
// EventType, DataVersion, ActorType/ActorID, EventCreatedAt, and Payload (the
// envelope data) describe the event: they are written to webhook_events by
// the first delivery of EventID and ignored afterwards.
type DeliveryDraft struct {
	TenantID         int64
	EndpointID       int64
	EventID          string
	EventType        string
	DataVersion      int32
	ActorType        string
	ActorID          string
	EventCreatedAt   time.Time
	URL              string
	Payload          json.RawMessage
	SecretCiphertext string
	MaxAttempts      int32
	TimeoutMS        int32
	RunAt            time.Time
	ReplayedFromID   int64
	// IsReplay inserts a new delivery for an existing event instead of taking
	// part in the per-endpoint dispatch deduplication.
	IsReplay bool
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
	DeliveryID int64
	// Attempt is the claimed attempt number; it fences the write.
	Attempt       int32
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

// CreateInboundSourceInput is the source configuration a caller may set.
type CreateInboundSourceInput struct {
	Name string
	Slug string
}

// UpdateInboundSourceInput replaces the mutable source configuration.
type UpdateInboundSourceInput struct {
	Name   string
	Slug   string
	Status string
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

// DeliveryJobPayload is the job payload for one delivery attempt.
type DeliveryJobPayload struct {
	DeliveryID int64 `json:"deliveryId"`
}

// EndpointStore covers endpoint CRUD and secret snapshots.
type EndpointStore interface {
	CreateEndpoint(ctx context.Context, draft CreateEndpointDraft) (*Endpoint, error)
	GetEndpoint(ctx context.Context, id int64) (*Endpoint, error)
	ListEndpoints(ctx context.Context, cur pagination.DBCursor) ([]Endpoint, error)
	UpdateEndpoint(ctx context.Context, draft UpdateEndpointDraft) (*Endpoint, error)
	DeleteEndpoint(ctx context.Context, id int64) error
	RotateEndpointSecret(ctx context.Context, id int64, ciphertext string) (*Endpoint, error)
	ActiveEndpoints(ctx context.Context, tenantID int64) ([]ActiveEndpoint, error)
	ActiveEndpointSecret(ctx context.Context, endpointID int64) (string, error)
	SigningSecrets(ctx context.Context, endpointID int64) (*SigningSecrets, error)
}

// DeliveryStore covers the outbox, attempts, claims, and retention.
type DeliveryStore interface {
	CreateDeliveryWithJob(ctx context.Context, draft DeliveryDraft) (deliveryID int64, created bool, err error)
	GetDelivery(ctx context.Context, id int64) (*Delivery, error)
	ListDeliveries(ctx context.Context, params ListDeliveriesParams) ([]Delivery, error)
	ClaimDelivery(ctx context.Context, id int64, lease time.Duration) (*Delivery, error)
	MarkDeliverySucceeded(ctx context.Context, id int64, attempt int32, statusCode int) error
	MarkDeliveryDead(ctx context.Context, id int64, attempt int32, statusCode int, reason string) error
	ReleaseDeliveryClaim(ctx context.Context, id int64, attempt int32) error
	ExpireUnclaimableDelivery(ctx context.Context, id int64) (bool, error)
	RescheduleDeliveryWithJob(ctx context.Context, draft RetryDraft) error
	CreateDeliveryAttempt(ctx context.Context, draft AttemptDraft) error
	ListDeliveryAttempts(ctx context.Context, deliveryID int64) ([]DeliveryAttempt, error)
	PurgeDeliveries(ctx context.Context, retentionDays int32) (int64, error)
	PurgeOrphanEvents(ctx context.Context, retentionDays int32) (int64, error)
}

// InboundStore covers inbound source CRUD and deduplicated events.
type InboundStore interface {
	CreateInboundSource(ctx context.Context, draft InboundSourceDraft) (*InboundSource, error)
	GetInboundSource(ctx context.Context, id int64) (*InboundSource, error)
	GetInboundSourceByID(ctx context.Context, id int64) (*InboundSource, error)
	ListInboundSources(ctx context.Context, cur pagination.DBCursor) ([]InboundSource, error)
	UpdateInboundSource(ctx context.Context, draft UpdateInboundSourceDraft) (*InboundSource, error)
	DeleteInboundSource(ctx context.Context, id int64) (*InboundSource, error)
	RotateInboundSourceSecret(ctx context.Context, id int64, ciphertext string) (*InboundSource, error)
	CreateInboundEvent(ctx context.Context, draft InboundEventDraft) (eventID int64, created bool, err error)
	GetInboundEventByExternalID(ctx context.Context, sourceID int64, externalEventID string) (*InboundEvent, error)
	ListInboundEvents(ctx context.Context, sourceID int64, cur pagination.DBCursor) ([]InboundEvent, error)
	PurgeInboundEvents(ctx context.Context, retentionDays int32) (int64, error)
}

// DispatchStore is the narrow surface the outbound dispatcher needs.
type DispatchStore interface {
	ActiveEndpoints(ctx context.Context, tenantID int64) ([]ActiveEndpoint, error)
	CreateDeliveryWithJob(ctx context.Context, draft DeliveryDraft) (deliveryID int64, created bool, err error)
}
