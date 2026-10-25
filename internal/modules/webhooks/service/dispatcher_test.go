package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

type fakeDispatchStore struct {
	endpoints  map[int64][]repository.ActiveEndpoint
	deliveries map[string]repository.DeliveryDraft
	callErr    error
}

func (f *fakeDispatchStore) ActiveEndpoints(_ context.Context, tenantID int64) ([]repository.ActiveEndpoint, error) {
	if f.callErr != nil {
		return nil, f.callErr
	}
	return f.endpoints[tenantID], nil
}

func (f *fakeDispatchStore) CreateDeliveryWithJob(_ context.Context, draft repository.DeliveryDraft) (deliveryID int64, created bool, err error) {
	if f.callErr != nil {
		return 0, false, f.callErr
	}
	if f.deliveries == nil {
		f.deliveries = make(map[string]repository.DeliveryDraft)
	}
	key := fmt.Sprintf("%d/%d/%s", draft.TenantID, draft.EndpointID, draft.EventID)
	if _, ok := f.deliveries[key]; ok {
		return 0, false, nil
	}
	f.deliveries[key] = draft
	return int64(len(f.deliveries)), true, nil
}

func activeEndpoint(id, tenantID int64, patterns ...string) repository.ActiveEndpoint {
	return repository.ActiveEndpoint{
		ID:               id,
		TenantID:         tenantID,
		URL:              fmt.Sprintf("https://receiver.example/%d", id),
		EventTypes:       patterns,
		MaxAttempts:      3,
		TimeoutMS:        1000,
		SecretCiphertext: "sealed",
	}
}

func testEvent(id, eventType string, tenantID int64, occurredAt time.Time) events.Event {
	return events.Event{
		ID:          id,
		Type:        eventType,
		DataVersion: 2,
		OccurredAt:  occurredAt,
		TenantID:    tenantID,
		Actor:       events.UserActor(5),
		Data:        json.RawMessage(`{"fileId":7}`),
	}
}

func TestDispatcher_FanoutAndDedupe(t *testing.T) {
	store := &fakeDispatchStore{
		endpoints: map[int64][]repository.ActiveEndpoint{
			tenant.DefaultID: {
				activeEndpoint(1, tenant.DefaultID, "file.*"),
				activeEndpoint(2, tenant.DefaultID, "file.uploaded"),
				activeEndpoint(3, tenant.DefaultID, "user.registered"),
			},
		},
	}
	dispatcher := NewDispatcher(store)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }

	event := testEvent("evt_1", "file.uploaded", tenant.DefaultID, now)
	created, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch(): %v", err)
	}
	if created != 2 {
		t.Fatalf("created = %d, want 2", created)
	}
	for _, endpointID := range []int64{1, 2} {
		draft, ok := store.deliveries[fmt.Sprintf("%d/%d/evt_1", tenant.DefaultID, endpointID)]
		if !ok {
			t.Fatalf("missing delivery for endpoint %d", endpointID)
		}
		if string(draft.Payload) != `{"fileId":7}` {
			t.Errorf("payload = %s", draft.Payload)
		}
		// The whole envelope is stored, so the worker can rebuild it.
		if draft.EventType != "file.uploaded" || draft.DataVersion != 2 || draft.ActorType != "user" || draft.ActorID != "5" {
			t.Errorf("envelope = %s v%d %s/%s", draft.EventType, draft.DataVersion, draft.ActorType, draft.ActorID)
		}
		if !draft.EventCreatedAt.Equal(now) {
			t.Errorf("event time = %v", draft.EventCreatedAt)
		}
		if draft.URL != fmt.Sprintf("https://receiver.example/%d", endpointID) {
			t.Errorf("url = %s", draft.URL)
		}
	}
	if _, ok := store.deliveries[fmt.Sprintf("%d/%d/evt_1", tenant.DefaultID, 3)]; ok {
		t.Fatal("non-matching endpoint received a delivery")
	}

	// The durable subscription is at-least-once: re-handling the same event
	// must be idempotent.
	if err := dispatcher.HandleEvent(context.Background(), event); err != nil {
		t.Fatalf("HandleEvent(): %v", err)
	}
	if len(store.deliveries) != 2 {
		t.Fatalf("deliveries after redelivery = %d, want 2", len(store.deliveries))
	}
}

// The tenant comes from the envelope, never from the job context.
func TestDispatcher_UsesEnvelopeTenant(t *testing.T) {
	store := &fakeDispatchStore{
		endpoints: map[int64][]repository.ActiveEndpoint{
			42: {activeEndpoint(9, 42, "*")},
			1:  {activeEndpoint(10, 1, "*")},
		},
	}
	dispatcher := NewDispatcher(store)
	ctx := tenant.WithID(context.Background(), 1)

	created, err := dispatcher.Dispatch(ctx, testEvent("evt_t", "user.registered", 42, time.Now()))
	if err != nil || created != 1 {
		t.Fatalf("Dispatch() = (%d, %v), want (1, nil)", created, err)
	}
	if _, ok := store.deliveries["42/9/evt_t"]; !ok {
		t.Fatalf("deliveries = %v, want endpoint 9 of tenant 42", store.deliveries)
	}
}

func TestDispatcher_InvalidEventsArePermanent(t *testing.T) {
	dispatcher := NewDispatcher(&fakeDispatchStore{})
	valid := testEvent("evt_v", "file.uploaded", 1, time.Now())

	cases := map[string]func(*events.Event){
		"no tenant":    func(e *events.Event) { e.TenantID = 0 },
		"no id":        func(e *events.Event) { e.ID = "" },
		"no actor":     func(e *events.Event) { e.Actor = events.Actor{} },
		"pattern type": func(e *events.Event) { e.Type = "file.*" },
		"no version":   func(e *events.Event) { e.DataVersion = 0 },
	}
	for name, mutate := range cases {
		event := valid
		mutate(&event)
		err := dispatcher.HandleEvent(context.Background(), event)
		if !errors.Is(err, ErrInvalidEvent) || !jobs.IsPermanent(err) {
			t.Errorf("%s: error = %v, want a permanent ErrInvalidEvent", name, err)
		}
	}

	// Store failures are transient and retried.
	failing := NewDispatcher(&fakeDispatchStore{callErr: errors.New("boom")})
	err := failing.HandleEvent(context.Background(), valid)
	if err == nil || jobs.IsPermanent(err) {
		t.Fatalf("store failure error = %v, want a retryable error", err)
	}
}
