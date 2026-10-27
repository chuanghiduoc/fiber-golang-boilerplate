// Package dto contains the webhooks HTTP request and response contracts.
package dto

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

// Optional is one member of an RFC 7396 merge-patch request body. The first
// name in the struct is the presence envelope; Schema makes the OpenAPI
// property describe T itself (plus null), so the document stays a plain
// resource patch instead of exposing the envelope.
type Optional[T any] patch.Field[T]

// UnmarshalJSON records presence, explicit null, and the replacement value.
func (o *Optional[T]) UnmarshalJSON(data []byte) error {
	return (*patch.Field[T])(o).UnmarshalJSON(data)
}

// Apply writes the member to target when it was present; null clears it.
func (o Optional[T]) Apply(target *T) {
	patch.Field[T](o).Apply(target)
}

// Schema documents the member as T, nullable for an explicit clear.
func (o Optional[T]) Schema(r huma.Registry) *huma.Schema {
	schema := huma.SchemaFromType(r, reflect.TypeFor[T]())
	schema.Nullable = true
	return schema
}

// CreateWebhookEndpointRequest creates an outbound endpoint. The response
// includes a signing secret, so the operation is never idempotency-cached.
type CreateWebhookEndpointRequest struct {
	URL         string   `json:"url" maxLength:"2048" doc:"Absolute http(s) receiver URL."`
	Description string   `json:"description,omitempty" maxLength:"255" doc:"Human-readable label."`
	EventTypes  []string `json:"eventTypes,omitempty" maxItems:"20" doc:"Event-type patterns (exact, prefix, or *); empty subscribes to everything."`
	Status      string   `json:"status,omitempty" enum:"active,disabled" doc:"Initial status; defaults to active."`
	MaxAttempts *int     `json:"maxAttempts,omitempty" minimum:"1" maximum:"50" doc:"Delivery attempt budget."`
	TimeoutMS   *int     `json:"timeoutMs,omitempty" minimum:"100" maximum:"120000" doc:"Per-attempt request timeout in milliseconds."`
}

// UpdateWebhookEndpointRequest is an RFC 7396 merge patch: an absent member is
// unchanged, an explicit null restores the default, and any other value
// replaces it.
type UpdateWebhookEndpointRequest struct {
	URL         Optional[string]   `json:"url,omitempty" doc:"Receiver URL."`
	Description Optional[string]   `json:"description,omitempty" doc:"Human-readable label."`
	EventTypes  Optional[[]string] `json:"eventTypes,omitempty" doc:"Replacement event-type patterns; null restores *."`
	Status      Optional[string]   `json:"status,omitempty" enum:"active,disabled" doc:"Endpoint status."`
	MaxAttempts Optional[*int]     `json:"maxAttempts,omitempty" minimum:"1" maximum:"50" doc:"Delivery attempt budget; null restores the default."`
	TimeoutMS   Optional[*int]     `json:"timeoutMs,omitempty" minimum:"100" maximum:"120000" doc:"Per-attempt timeout in milliseconds; null restores the default."`
}

// WebhookEndpointResponse is the public outbound endpoint view (never includes
// the secret or its ciphertext).
type WebhookEndpointResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenantId"`
	URL         string     `json:"url"`
	Description string     `json:"description"`
	EventTypes  []string   `json:"eventTypes"`
	Status      string     `json:"status" enum:"active,disabled"`
	MaxAttempts int32      `json:"maxAttempts"`
	TimeoutMS   int32      `json:"timeoutMs"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	DeletedAt   *time.Time `json:"deletedAt,omitempty"`
}

// WebhookEndpointSecretResponse is returned exactly once when an endpoint
// secret is created. Rotation returns SecretResponse only.
type WebhookEndpointSecretResponse struct {
	WebhookEndpointResponse
	Secret string `json:"secret" doc:"Standard Webhooks signing secret; shown once."`
}

// RotateSecretRequest is the optional body of either rotate-secret operation.
type RotateSecretRequest struct {
	ExpirePrevious bool `json:"expirePrevious,omitempty" doc:"End the previous secret's co-signing grace immediately."`
}

// SecretResponse is the one-time result of a secret rotation.
type SecretResponse struct {
	Secret string `json:"secret" doc:"New Standard Webhooks secret; shown once."`
}

// WebhookDeliveryResponse is one delivery row. Payload is omitted from list
// responses and included on single-resource reads/replays.
type WebhookDeliveryResponse struct {
	ID             uuid.UUID       `json:"id"`
	TenantID       uuid.UUID       `json:"tenantId"`
	EndpointID     uuid.UUID       `json:"endpointId"`
	EventID        uuid.UUID       `json:"eventId"`
	EventType      string          `json:"eventType"`
	Status         string          `json:"status" enum:"pending,succeeded,dead"`
	Attempts       int32           `json:"attempts"`
	MaxAttempts    int32           `json:"maxAttempts"`
	NextAttemptAt  time.Time       `json:"nextAttemptAt"`
	LastStatusCode *int            `json:"lastStatusCode,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	DeliveredAt    *time.Time      `json:"deliveredAt,omitempty"`
	ReplayedFromID *uuid.UUID      `json:"replayedFromId,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// WebhookAttemptResponse records one HTTP attempt.
type WebhookAttemptResponse struct {
	ID         uuid.UUID `json:"id"`
	DeliveryID uuid.UUID `json:"deliveryId"`
	Attempt    int32     `json:"attempt"`
	StatusCode *int      `json:"statusCode,omitempty"`
	DurationMS int32     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// CreateInboundSourceRequest creates a public HMAC-authenticated source.
type CreateInboundSourceRequest struct {
	Name   string `json:"name" minLength:"1" maxLength:"255" doc:"Human-readable source name."`
	Slug   string `json:"slug" pattern:"^[a-z0-9-]{1,63}$" doc:"Stable lowercase slug."`
	Status string `json:"status,omitempty" enum:"active,disabled" doc:"Initial status; defaults to active."`
}

// UpdateInboundSourceRequest is an RFC 7396 merge patch for a source.
type UpdateInboundSourceRequest struct {
	Name   Optional[string] `json:"name,omitempty" doc:"Human-readable source name."`
	Slug   Optional[string] `json:"slug,omitempty" doc:"Stable lowercase slug."`
	Status Optional[string] `json:"status,omitempty" enum:"active,disabled" doc:"Source status."`
}

// InboundSourceResponse is the public source view without the secret.
type InboundSourceResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenantId"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Status      string     `json:"status" enum:"active,disabled"`
	ReceiverURL string     `json:"receiverUrl"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	DeletedAt   *time.Time `json:"deletedAt,omitempty"`
}

// InboundSourceSecretResponse is returned exactly once when a source secret is
// created. Rotation returns SecretResponse only.
type InboundSourceSecretResponse struct {
	InboundSourceResponse
	Secret string `json:"secret" doc:"Standard Webhooks verification secret; shown once."`
}

// InboundEventResponse is a stored inbound event.
type InboundEventResponse struct {
	ID              uuid.UUID       `json:"id"`
	TenantID        uuid.UUID       `json:"tenantId"`
	SourceID        uuid.UUID       `json:"sourceId"`
	ExternalEventID string          `json:"eventId"`
	EventType       string          `json:"eventType"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	ReceivedAt      time.Time       `json:"receivedAt"`
	Duplicate       bool            `json:"duplicate"`
}

// InboundReceipt is the public receiver's acknowledgement. Stored payloads are
// intentionally not echoed; duplicate is true for a repeated (source, event
// id) delivery.
type InboundReceipt struct {
	ID         uuid.UUID `json:"id"`
	TenantID   uuid.UUID `json:"tenantId"`
	SourceID   uuid.UUID `json:"sourceId"`
	EventID    string    `json:"eventId"`
	EventType  string    `json:"eventType"`
	ReceivedAt time.Time `json:"receivedAt"`
	Duplicate  bool      `json:"duplicate"`
}
