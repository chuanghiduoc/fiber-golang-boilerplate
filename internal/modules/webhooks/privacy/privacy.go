// Package privacy implements the webhooks module's privacy contract for
// stored event payloads and delivery history that name a user. Endpoint
// configuration itself is tenant-wide, not per-user.
package privacy

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/google/uuid"

	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// exportJSONOverhead reserves room for array separators, field names, and the
// budget object. Two collections of up to DefaultRowLimit rows need roughly
// two thousand separators, so a small fixed allowance would undercount the
// final document.
const exportJSONOverhead int64 = 4096

// Event is one stored outbound event attributed to a user.
type Event struct {
	ID          uuid.UUID
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
	ID             uuid.UUID
	EventID        uuid.UUID
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
	ListEvents(ctx context.Context, tenantID, userID uuid.UUID, cursor pagination.DBCursor) ([]Event, error)
	ListDeliveries(ctx context.Context, tenantID, userID uuid.UUID, cursor pagination.DBCursor) ([]Delivery, error)
	DeleteEvents(ctx context.Context, tenantID, userID uuid.UUID) error
}

// Provider implements privacy.Exporter and privacy.Eraser.
type Provider struct {
	store     Store
	rowLimit  int32
	byteLimit int64
}

func New(store Store) *Provider {
	return &Provider{
		store:     store,
		rowLimit:  privacycontracts.DefaultRowLimit,
		byteLimit: privacycontracts.DefaultByteLimit,
	}
}

type eventExport struct {
	ID          uuid.UUID       `json:"id"`
	Type        string          `json:"type"`
	DataVersion int32           `json:"dataVersion"`
	ActorType   string          `json:"actorType"`
	ActorID     string          `json:"actorId,omitempty"`
	OccurredAt  time.Time       `json:"occurredAt"`
	Payload     json.RawMessage `json:"payload"`
}

type deliveryExport struct {
	ID             uuid.UUID  `json:"id"`
	EventID        uuid.UUID  `json:"eventId"`
	URL            string     `json:"url"`
	Status         string     `json:"status"`
	Attempts       int32      `json:"attempts"`
	MaxAttempts    int32      `json:"maxAttempts"`
	LastStatusCode *int32     `json:"lastStatusCode,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
	DeliveredAt    *time.Time `json:"deliveredAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
}

type exportDocument struct {
	Events     []eventExport           `json:"events"`
	Deliveries []deliveryExport        `json:"deliveries"`
	Budget     privacycontracts.Budget `json:"budget"`
}

func (p *Provider) Export(ctx context.Context, tenantID, userID uuid.UUID) (privacycontracts.Section, error) {
	events, eventsTruncated, err := p.listEvents(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	deliveries, deliveriesTruncated, err := p.listDeliveries(ctx, tenantID, userID)
	if err != nil {
		return privacycontracts.Section{}, err
	}

	out := exportDocument{
		Events:     make([]eventExport, 0, len(events)),
		Deliveries: make([]deliveryExport, 0, len(deliveries)),
	}
	truncated := eventsTruncated || deliveriesTruncated
	var used int64

	for _, event := range events {
		payload := event.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		if !p.fitsBudget(used, len(payload)) {
			truncated = true
			break
		}
		item := eventExport{
			ID: event.ID, Type: event.Type, DataVersion: event.DataVersion,
			ActorType: event.ActorType, ActorID: event.ActorID,
			OccurredAt: event.OccurredAt, Payload: payload,
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		if !p.fitsBudget(used, len(encoded)) {
			truncated = true
			break
		}
		out.Events = append(out.Events, item)
		used += int64(len(encoded))
	}

	for _, delivery := range deliveries {
		item := deliveryExport(delivery)
		encoded, err := json.Marshal(item)
		if err != nil {
			return privacycontracts.Section{}, err
		}
		if !p.fitsBudget(used, len(encoded)) {
			truncated = true
			break
		}
		out.Deliveries = append(out.Deliveries, item)
		used += int64(len(encoded))
	}

	out.Budget = privacycontracts.Budget{
		RowLimit: p.rowLimit, ByteLimit: p.byteLimit,
		Returned: len(out.Events) + len(out.Deliveries), Truncated: truncated,
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return privacycontracts.Section{}, err
	}
	return privacycontracts.Section{Name: "webhooks", Data: raw, Truncated: truncated}, nil
}

func (p *Provider) fitsBudget(used int64, itemBytes int) bool {
	if p.byteLimit <= 0 {
		return true
	}
	return used+int64(itemBytes)+exportJSONOverhead <= p.byteLimit
}

func (p *Provider) listEvents(ctx context.Context, tenantID, userID uuid.UUID) ([]Event, bool, error) {
	limit, fetchLimit := p.pageLimit()
	if limit == 0 {
		return []Event{}, false, nil
	}
	page, err := p.store.ListEvents(ctx, tenantID, userID, pagination.DBCursor{Limit: fetchLimit})
	if err != nil {
		return nil, false, err
	}
	truncated := len(page) > int(limit)
	if truncated {
		page = page[:limit]
	}
	return page, truncated, nil
}

func (p *Provider) listDeliveries(ctx context.Context, tenantID, userID uuid.UUID) ([]Delivery, bool, error) {
	limit, fetchLimit := p.pageLimit()
	if limit == 0 {
		return []Delivery{}, false, nil
	}
	page, err := p.store.ListDeliveries(ctx, tenantID, userID, pagination.DBCursor{Limit: fetchLimit})
	if err != nil {
		return nil, false, err
	}
	truncated := len(page) > int(limit)
	if truncated {
		page = page[:limit]
	}
	return page, truncated, nil
}

// pageLimit asks one extra row so an exact-limit result is not mistaken for a
// truncated one. Both collections are one query; the exporter never walks an
// unbounded history into memory.
func (p *Provider) pageLimit() (limit, fetchLimit int32) {
	if p.rowLimit <= 0 {
		return 0, 0
	}
	if p.rowLimit == math.MaxInt32 {
		return p.rowLimit, p.rowLimit
	}
	return p.rowLimit, p.rowLimit + 1
}

func (p *Provider) Erase(ctx context.Context, tenantID, userID uuid.UUID) error {
	return p.store.DeleteEvents(ctx, tenantID, userID)
}
