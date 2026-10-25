// Package privacy implements the webhooks module's privacy contract for
// stored event payloads and delivery history that name a user. Endpoint
// configuration itself is tenant-wide, not per-user.
package privacy

import (
	"context"
	"encoding/json"
	"time"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
)

// Event is one stored outbound event attributed to a user.
type Event struct {
	ID          string
	Type        string
	DataVersion int32
	ActorType   string
	ActorID     string
	OccurredAt  time.Time
	Payload     json.RawMessage
	CreatedAt   time.Time
}

// Delivery is one delivery of such an event.
type Delivery struct {
	ID             int64
	EventID        string
	URL            string
	Status         string
	Attempts       int32
	MaxAttempts    int32
	LastStatusCode *int32
	LastError      string
	DeliveredAt    *time.Time
	CreatedAt      time.Time
}

// Store is the module's privacy persistence port.
type Store interface {
	ListEvents(ctx context.Context, tenantID, userID int64) ([]Event, error)
	ListDeliveries(ctx context.Context, tenantID, userID int64) ([]Delivery, error)
	DeleteEvents(ctx context.Context, tenantID, userID int64) error
}

// Provider implements privacy.Exporter and privacy.Eraser.
type Provider struct {
	store Store
}

func New(store Store) *Provider { return &Provider{store: store} }

func (p *Provider) Export(ctx context.Context, tenantID, userID int64) (privacycontracts.Section, error) {
	events, err := p.store.ListEvents(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	deliveries, err := p.store.ListDeliveries(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}

	type eventExport struct {
		ID          string          `json:"id"`
		Type        string          `json:"type"`
		DataVersion int32           `json:"dataVersion"`
		ActorType   string          `json:"actorType"`
		ActorID     string          `json:"actorId,omitempty"`
		OccurredAt  time.Time       `json:"occurredAt"`
		Payload     json.RawMessage `json:"payload"`
	}
	type deliveryExport struct {
		ID             int64      `json:"id"`
		EventID        string     `json:"eventId"`
		URL            string     `json:"url"`
		Status         string     `json:"status"`
		Attempts       int32      `json:"attempts"`
		MaxAttempts    int32      `json:"maxAttempts"`
		LastStatusCode *int32     `json:"lastStatusCode,omitempty"`
		LastError      string     `json:"lastError,omitempty"`
		DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
		CreatedAt      time.Time  `json:"createdAt"`
	}
	out := struct {
		Events     []eventExport    `json:"events"`
		Deliveries []deliveryExport `json:"deliveries"`
	}{
		Events:     make([]eventExport, len(events)),
		Deliveries: make([]deliveryExport, len(deliveries)),
	}
	for i, event := range events {
		payload := event.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		out.Events[i] = eventExport{
			ID: event.ID, Type: event.Type, DataVersion: event.DataVersion,
			ActorType: event.ActorType, ActorID: event.ActorID,
			OccurredAt: event.OccurredAt, Payload: payload,
		}
	}
	for i, delivery := range deliveries {
		out.Deliveries[i] = deliveryExport(delivery)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "webhooks", Data: raw}, nil
}

func (p *Provider) Erase(ctx context.Context, tenantID, userID int64) error {
	return p.store.DeleteEvents(ctx, tenantID, userID)
}
