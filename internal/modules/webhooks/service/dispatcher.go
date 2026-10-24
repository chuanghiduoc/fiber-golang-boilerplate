package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

const (
	// DispatchJobKind is the durable job that fans one event out to the
	// tenant's matching endpoints. DispatchTx enqueues it inside the
	// producer's transaction.
	DispatchJobKind = "webhook.dispatch"
	// dispatchJobMaxAttempts matches the delivery job budget: a fan-out only
	// fails on database errors, which are usually transient.
	dispatchJobMaxAttempts = 20
)

// TxQueue is implemented by platform/jobs.Queue.
type TxQueue interface {
	EnqueueOnceTx(ctx context.Context, tx pgx.Tx, job jobs.Job) (int64, bool, error)
}

// Dispatcher fans domain events out to the owning tenant's matching active
// endpoints. Delivery rows and their first durable job commit atomically, and
// (tenant, endpoint, event ID) is unique, so retrying Dispatch is safe.
type Dispatcher struct {
	store repository.DispatchStore
	queue TxQueue
	now   func() time.Time
}

var _ webhookcontracts.Dispatcher = (*Dispatcher)(nil)

// NewDispatcher returns a dispatcher backed by store. queue is required only
// for DispatchTx.
func NewDispatcher(store repository.DispatchStore, queue TxQueue) *Dispatcher {
	return &Dispatcher{
		store: store,
		queue: queue,
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// dispatchJobPayload is a fully prepared event: its ID is fixed at enqueue
// time, so every retry of the fan-out job deduplicates on the same deliveries.
type dispatchJobPayload struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	TenantID   int64           `json:"tenantId"`
	OccurredAt time.Time       `json:"occurredAt"`
	Data       json.RawMessage `json:"data"`
}

// DispatchTx records events in the producer's transaction as one
// webhook.dispatch job each, so a webhook exists if and only if the business
// change commits. The job later runs Dispatch with the event's stable ID.
// tx must be the producer's open pgx transaction.
func (d *Dispatcher) DispatchTx(ctx context.Context, tx pgx.Tx, events ...webhookcontracts.Event) error {
	if d == nil || d.queue == nil {
		return errors.New("webhooks: dispatcher job queue is nil")
	}
	if tx == nil {
		return jobs.ErrNoTx
	}
	for i := range events {
		event, err := d.prepare(ctx, events[i])
		if err != nil {
			return err
		}
		payload, err := json.Marshal(dispatchJobPayload{
			ID:         event.ID,
			Type:       event.Type,
			TenantID:   event.TenantID,
			OccurredAt: event.OccurredAt,
			Data:       event.Data,
		})
		if err != nil {
			return fmt.Errorf("webhooks: encode dispatch job: %w", err)
		}
		if _, _, err := d.queue.EnqueueOnceTx(ctx, tx, jobs.Job{
			TenantID:       event.TenantID,
			Kind:           DispatchJobKind,
			Payload:        payload,
			MaxAttempts:    dispatchJobMaxAttempts,
			IdempotencyKey: DispatchJobKind + ":" + event.ID,
		}); err != nil {
			return fmt.Errorf("webhooks: enqueue dispatch job: %w", err)
		}
	}
	return nil
}

// HandleDispatchJob is registered on the platform jobs worker under
// DispatchJobKind. It is idempotent: deliveries are unique per endpoint and
// event ID, so a retried job only creates the deliveries still missing.
func (d *Dispatcher) HandleDispatchJob(ctx context.Context, payload json.RawMessage) error {
	var job dispatchJobPayload
	if err := json.Unmarshal(payload, &job); err != nil {
		return fmt.Errorf("webhooks: invalid dispatch job payload: %w", err)
	}
	if job.ID == "" || job.TenantID <= 0 {
		return fmt.Errorf("%w: dispatch job requires an event id and tenant", webhookcontracts.ErrInvalidEvent)
	}
	_, err := d.Dispatch(ctx, webhookcontracts.Event{
		ID:         job.ID,
		Type:       job.Type,
		TenantID:   job.TenantID,
		OccurredAt: job.OccurredAt,
		Data:       job.Data,
	})
	return err
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
		event, err := d.prepare(ctx, events[i])
		if err != nil {
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
				TimeoutMS:        endpoint.TimeoutMS,
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

// prepare resolves the tenant, applies defaults, assigns an ID when the
// producer left it empty, and validates the event.
func (d *Dispatcher) prepare(ctx context.Context, event webhookcontracts.Event) (webhookcontracts.Event, error) {
	if event.TenantID == 0 {
		event.TenantID = tenant.EffectiveID(ctx)
	}
	if event.TenantID <= 0 {
		return event, fmt.Errorf("%w: tenant id is required", webhookcontracts.ErrInvalidEvent)
	}
	event = event.Normalized(d.now())
	if event.ID == "" {
		event.ID = "evt_" + randomHex(12)
	}
	if err := event.Validate(); err != nil {
		return event, err
	}
	return event, nil
}
