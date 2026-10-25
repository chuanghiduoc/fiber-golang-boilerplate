// Package dto contains the webhooks HTTP request and response contracts.
package dto

import (
	"encoding/json"
	"time"
)

// CreateEndpointRequest creates an outbound endpoint. The response includes a
// signing secret, so the route is not wrapped in idempotency caching.
type CreateEndpointRequest struct {
	URL         string   `json:"url" validate:"required,max=2048"`
	Description string   `json:"description" validate:"omitempty,max=255"`
	EventTypes  []string `json:"eventTypes" validate:"omitempty,max=20,dive,min=1,max=128"`
	MaxAttempts *int     `json:"maxAttempts" validate:"omitempty,min=1,max=50"`
	TimeoutMS   *int     `json:"timeoutMs" validate:"omitempty,min=100,max=120000"`
}

// UpdateEndpointRequest replaces all mutable endpoint configuration.
type UpdateEndpointRequest struct {
	URL         string   `json:"url" validate:"required,max=2048"`
	Description string   `json:"description" validate:"omitempty,max=255"`
	EventTypes  []string `json:"eventTypes" validate:"omitempty,max=20,dive,min=1,max=128"`
	Status      string   `json:"status" validate:"required,oneof=active disabled"`
	MaxAttempts *int     `json:"maxAttempts" validate:"omitempty,min=1,max=50"`
	TimeoutMS   *int     `json:"timeoutMs" validate:"omitempty,min=100,max=120000"`
}

// EndpointResponse is the public outbound endpoint view (never includes the
// secret or its ciphertext).
type EndpointResponse struct {
	ID          int64      `json:"id"`
	TenantID    int64      `json:"tenantId"`
	URL         string     `json:"url"`
	Description string     `json:"description"`
	EventTypes  []string   `json:"eventTypes"`
	Status      string     `json:"status"`
	MaxAttempts int32      `json:"maxAttempts"`
	TimeoutMS   int32      `json:"timeoutMs"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	DeletedAt   *time.Time `json:"deletedAt,omitempty"`
}

// EndpointSecretResponse is returned exactly once when a secret is created or
// rotated.
type EndpointSecretResponse struct {
	EndpointResponse
	Secret string `json:"secret"`
}

// DeliveryResponse is one delivery row. Payload is omitted from list responses
// and included on single-resource reads/replays.
type DeliveryResponse struct {
	ID             int64           `json:"id"`
	TenantID       int64           `json:"tenantId"`
	EndpointID     int64           `json:"endpointId"`
	EventID        string          `json:"eventId"`
	EventType      string          `json:"eventType"`
	Status         string          `json:"status"`
	Attempts       int32           `json:"attempts"`
	MaxAttempts    int32           `json:"maxAttempts"`
	NextAttemptAt  time.Time       `json:"nextAttemptAt"`
	LastStatusCode *int            `json:"lastStatusCode,omitempty"`
	LastError      string          `json:"lastError,omitempty"`
	DeliveredAt    *time.Time      `json:"deliveredAt,omitempty"`
	ReplayedFromID *int64          `json:"replayedFromId,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty" swaggertype:"object"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// DeliveryAttemptResponse records one HTTP attempt.
type DeliveryAttemptResponse struct {
	ID         int64     `json:"id"`
	DeliveryID int64     `json:"deliveryId"`
	Attempt    int32     `json:"attempt"`
	StatusCode *int      `json:"statusCode,omitempty"`
	DurationMS int32     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// CreateInboundSourceRequest creates a public HMAC-authenticated source.
type CreateInboundSourceRequest struct {
	Name string `json:"name" validate:"required,notblank,max=255"`
	Slug string `json:"slug" validate:"required,min=1,max=63"`
}

// UpdateInboundSourceRequest replaces the source name, slug, and status.
type UpdateInboundSourceRequest struct {
	Name   string `json:"name" validate:"required,notblank,max=255"`
	Slug   string `json:"slug" validate:"required,min=1,max=63"`
	Status string `json:"status" validate:"required,oneof=active disabled"`
}

// InboundSourceResponse is the public source view without the secret.
type InboundSourceResponse struct {
	ID          int64      `json:"id"`
	TenantID    int64      `json:"tenantId"`
	Name        string     `json:"name"`
	Slug        string     `json:"slug"`
	Status      string     `json:"status"`
	ReceiverURL string     `json:"receiverUrl"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	DeletedAt   *time.Time `json:"deletedAt,omitempty"`
}

// InboundSourceSecretResponse is returned exactly once on create/rotation.
type InboundSourceSecretResponse struct {
	InboundSourceResponse
	Secret string `json:"secret"`
}

// InboundEventResponse is a stored inbound event. Duplicate is true when the
// receiver found an existing event for the same source and external ID.
type InboundEventResponse struct {
	ID              int64           `json:"id"`
	TenantID        int64           `json:"tenantId"`
	SourceID        int64           `json:"sourceId"`
	ExternalEventID string          `json:"eventId"`
	EventType       string          `json:"eventType"`
	Payload         json.RawMessage `json:"payload,omitempty" swaggertype:"object"`
	ReceivedAt      time.Time       `json:"receivedAt"`
	Duplicate       bool            `json:"duplicate"`
}

// InboundReceipt is the public receiver's minimal acknowledgement. Stored
// payloads are intentionally not echoed.
type InboundReceipt struct {
	ID         int64     `json:"id"`
	TenantID   int64     `json:"tenantId"`
	SourceID   int64     `json:"sourceId"`
	EventID    string    `json:"eventId"`
	EventType  string    `json:"eventType"`
	ReceivedAt time.Time `json:"receivedAt"`
	Duplicate  bool      `json:"duplicate"`
}
