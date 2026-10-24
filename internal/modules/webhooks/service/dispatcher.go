package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// Dispatcher fans domain events out to the owning tenant's matching active
// endpoints. Delivery rows and their first durable job commit atomically, and
// (tenant, endpoint, event ID) is unique, so retrying Dispatch is safe.
type Dispatcher struct {
	store repository.DispatchStore
	now   func() time.Time
}

var _ webhookcontracts.Dispatcher = (*Dispatcher)(nil)

// NewDispatcher returns a dispatcher backed by store.
func NewDispatcher(store repository.DispatchStore) *Dispatcher {
	return &Dispatcher{
		store: store,
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// Dispatch implements contracts.Dispatcher.
func (d *Dispatcher) Dispatch(ctx context.Context, events ...webhookcontracts.Event) (int, error) {
	if d == nil || d.store == nil {
		return 0, fmt.Errorf("webhooks: dispatcher store is nil")
	}
	if len(events) == 0 {
		return 0, nil
	}

	endpointCache := make(map[int64][]repository.ActiveEndpoint)
	created := 0
	for i := range events {
		event := events[i]
		if event.TenantID == 0 {
			event.TenantID = tenant.EffectiveID(ctx)
		}
		if event.TenantID <= 0 {
			return created, fmt.Errorf("%w: tenant id is required", webhookcontracts.ErrInvalidEvent)
		}
		event = event.Normalized(d.now())
		if event.ID == "" {
			event.ID = "evt_" + randomHex(12)
		}
		if err := event.Validate(); err != nil {
			return created, err
		}

		endpoints, ok := endpointCache[event.TenantID]
		if !ok {
			var err error
			endpoints, err = d.store.ActiveEndpoints(ctx, event.TenantID)
			if err != nil {
				return created, fmt.Errorf("webhooks: load active endpoints: %w", err)
			}
			endpointCache[event.TenantID] = endpoints
		}

		for _, endpoint := range endpoints {
			if !webhookcontracts.Matches(endpoint.EventTypes, event.Type) {
				continue
			}
			_, inserted, err := d.store.CreateDeliveryWithJob(ctx, repository.DeliveryDraft{
				TenantID:         event.TenantID,
				EndpointID:       endpoint.ID,
				EventID:          event.ID,
				EventType:        event.Type,
				EventCreatedAt:   event.OccurredAt,
				URL:              endpoint.URL,
				Payload:          event.Data,
				SecretCiphertext: endpoint.SecretCiphertext,
				MaxAttempts:      endpoint.MaxAttempts,
				RunAt:            d.now(),
			})
			if err != nil {
				return created, fmt.Errorf("webhooks: create delivery for endpoint %d: %w", endpoint.ID, err)
			}
			if inserted {
				created++
			}
		}
	}

	if created > 0 {
		slog.Debug("webhooks: dispatched events", slog.Int("deliveries", created))
	}
	return created, nil
}
