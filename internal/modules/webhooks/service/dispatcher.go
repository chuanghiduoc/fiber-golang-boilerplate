package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
)

// ErrInvalidEvent is returned (wrapped) for an event that can never be
// dispatched; the durable subscription dead-letters it.
var ErrInvalidEvent = errors.New("webhooks: invalid event")

// maxDataVersion is the largest dataVersion webhook_events.data_version holds.
const maxDataVersion = math.MaxInt32

// Dispatcher fans committed domain events out to the owning tenant's matching
// active endpoints. It runs as a durable event subscriber (HandleEvent):
// producers publish with events.Bus.PublishTx and never know about webhooks.
// Delivery rows and their first durable job commit atomically, and
// (tenant, endpoint, event ID) is unique, so a retried subscriber job only
// creates the deliveries still missing.
type Dispatcher struct {
	store DispatchStore
	now   func() time.Time
}

// NewDispatcher returns a dispatcher backed by store.
func NewDispatcher(store DispatchStore) *Dispatcher {
	return &Dispatcher{
		store: store,
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// HandleEvent is the durable events.Handler subscribed to every webhook event
// type (catalog.SubscribedEvents). An invalid event can never be dispatched,
// so it dead-letters without spending the retry budget; store errors retry.
func (d *Dispatcher) HandleEvent(ctx context.Context, event events.Event) error {
	_, err := d.Dispatch(ctx, event)
	if errors.Is(err, ErrInvalidEvent) {
		return jobs.Permanent(err)
	}
	return err
}

// Dispatch creates one delivery per matching active endpoint and returns how
// many were newly created. The envelope is stored once in webhook_events.
func (d *Dispatcher) Dispatch(ctx context.Context, event events.Event) (int, error) {
	if d == nil || d.store == nil {
		return 0, errors.New("webhooks: dispatcher store is nil")
	}
	if err := validateDispatchEvent(event); err != nil {
		return 0, err
	}
	endpoints, err := d.store.ActiveEndpoints(ctx, event.TenantID)
	if err != nil {
		return 0, fmt.Errorf("webhooks: load active endpoints: %w", err)
	}

	created := 0
	for _, endpoint := range endpoints {
		if !Matches(endpoint.EventTypes, event.Type) {
			continue
		}
		_, inserted, err := d.store.CreateDeliveryWithJob(ctx, DeliveryDraft{
			TenantID:       event.TenantID,
			EndpointID:     endpoint.ID,
			EventID:        event.ID,
			EventType:      event.Type,
			DataVersion:    int32(event.DataVersion), // #nosec G115 -- validated 1..math.MaxInt32
			ActorType:      event.Actor.Type,
			ActorID:        event.Actor.ID,
			SubjectUserIDs: subjectUserIDsFor(event),
			EventCreatedAt: event.OccurredAt,
			URL:            endpoint.URL,
			Payload:        event.Data,
			SecretID:       endpoint.SecretID,
			MaxAttempts:    endpoint.MaxAttempts,
			TimeoutMS:      endpoint.TimeoutMS,
			RunAt:          d.now(),
		})
		if err != nil {
			return created, fmt.Errorf("webhooks: create delivery for endpoint %s: %w", endpoint.ID, err)
		}
		if inserted {
			created++
		}
	}
	if created > 0 {
		slog.Debug("webhooks: dispatched event",
			slog.String("event", event.Type),
			slog.Int("deliveries", created),
		)
	}
	return created, nil
}

// subjectUserIDsFor merges the authenticated user actor, the producer's
// explicit subjects, and the user-id keys shipped by the current event
// contracts. The payload fallback also preserves attribution for events that
// were already queued when this metadata was introduced. Duplicates are
// removed so the persisted array stays canonical.
func subjectUserIDsFor(event events.Event) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(event.SubjectUserIDs)+1)
	seen := make(map[uuid.UUID]struct{}, cap(ids))
	add := func(id uuid.UUID) {
		if id == uuid.Nil {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, id := range event.SubjectUserIDs {
		add(id)
	}
	for _, id := range payloadSubjectUserIDs(event.Data) {
		add(id)
	}
	if event.Actor.Type == events.ActorUser {
		if id, err := uuid.Parse(event.Actor.ID); err == nil {
			add(id)
		}
	}
	return ids
}

// payloadSubjectUserIDs reads the top-level user-id fields used by the events
// shipped before subject_user_ids existed. Nested payloads are covered by the
// producer setting SubjectUserIDs explicitly.
func payloadSubjectUserIDs(data json.RawMessage) []uuid.UUID {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	keys := [...]string{
		"userId", "ownerId", "accountId", "memberId", "subjectId", "targetUserId",
	}
	ids := make([]uuid.UUID, 0, len(keys))
	for _, key := range keys {
		raw, ok := payload[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			continue
		}
		if id, err := uuid.Parse(value); err == nil && id != uuid.Nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// validateDispatchEvent checks what the bus cannot: a webhook event must
// belong to a tenant and carry a stable id, a type endpoints can subscribe
// to, and an actor (the bus fills one in on publish).
func validateDispatchEvent(event events.Event) error {
	if err := event.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	switch {
	case event.TenantID == uuid.Nil:
		return fmt.Errorf("%w: tenant id is required", ErrInvalidEvent)
	case event.ID == uuid.Nil:
		return fmt.Errorf("%w: id is required", ErrInvalidEvent)
	case event.Actor.Type == "":
		return fmt.Errorf("%w: actor is required", ErrInvalidEvent)
	case event.Actor.Type != events.ActorSystem:
		if _, err := uuid.Parse(event.Actor.ID); err != nil {
			return fmt.Errorf("%w: actor id must be a UUID", ErrInvalidEvent)
		}
	case event.DataVersion > maxDataVersion:
		return fmt.Errorf("%w: dataVersion is out of range", ErrInvalidEvent)
	case !ValidType(event.Type):
		return fmt.Errorf("%w: type %q is not subscribable", ErrInvalidEvent, event.Type)
	}
	return nil
}
