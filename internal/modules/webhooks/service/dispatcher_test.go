package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

type fakeDispatchStore struct {
	endpoints  map[uuid.UUID][]ActiveEndpoint
	deliveries map[string]DeliveryDraft
	callErr    error
}

func (f *fakeDispatchStore) ActiveEndpoints(_ context.Context, tenantID uuid.UUID) ([]ActiveEndpoint, error) {
	if f.callErr != nil {
		return nil, f.callErr
	}
	return f.endpoints[tenantID], nil
}

func (f *fakeDispatchStore) CreateDeliveryWithJob(_ context.Context, draft DeliveryDraft) (deliveryID uuid.UUID, created bool, err error) {
	if f.callErr != nil {
		return uuid.Nil, false, f.callErr
	}
	if f.deliveries == nil {
		f.deliveries = make(map[string]DeliveryDraft)
	}
	key := deliveryKey(draft.TenantID, draft.EndpointID, draft.EventID)
	if _, ok := f.deliveries[key]; ok {
		return uuid.Nil, false, nil
	}
	f.deliveries[key] = draft
	return testID(len(f.deliveries)), true, nil
}

func deliveryKey(tenantID, endpointID, eventID uuid.UUID) string {
	return fmt.Sprintf("%s/%s/%s", tenantID, endpointID, eventID)
}

func activeEndpoint(id, tenantID uuid.UUID, patterns ...string) ActiveEndpoint {
	return ActiveEndpoint{
		ID:          id,
		TenantID:    tenantID,
		URL:         fmt.Sprintf("https://receiver.example/%s", id),
		EventTypes:  patterns,
		MaxAttempts: 3,
		TimeoutMS:   1000,
		SecretID:    testID(99),
	}
}

func testEvent(id uuid.UUID, eventType string, tenantID uuid.UUID, occurredAt time.Time) events.Event {
	return events.Event{
		ID:          id,
		Type:        eventType,
		DataVersion: 2,
		OccurredAt:  occurredAt,
		TenantID:    tenantID,
		Actor:       events.UserActor(testID(5)),
		Data:        json.RawMessage(`{"fileId":7}`),
	}
}

func TestDispatcher_FanoutAndDedupe(t *testing.T) {
	store := &fakeDispatchStore{
		endpoints: map[uuid.UUID][]ActiveEndpoint{
			tenant.DefaultID: {
				activeEndpoint(testID(1), tenant.DefaultID, "file.*"),
				activeEndpoint(testID(2), tenant.DefaultID, "file.uploaded"),
				activeEndpoint(testID(3), tenant.DefaultID, "user.registered"),
			},
		},
	}
	dispatcher := NewDispatcher(store)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }

	event := testEvent(testID(11), "file.uploaded", tenant.DefaultID, now)
	created, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch(): %v", err)
	}
	if created != 2 {
		t.Fatalf("created = %d, want 2", created)
	}
	for _, endpointID := range []uuid.UUID{testID(1), testID(2)} {
		draft, ok := store.deliveries[deliveryKey(tenant.DefaultID, endpointID, testID(11))]
		if !ok {
			t.Fatalf("missing delivery for endpoint %s", endpointID)
		}
		if string(draft.Payload) != `{"fileId":7}` {
			t.Errorf("payload = %s", draft.Payload)
		}
		// The whole envelope is stored, so the worker can rebuild it.
		if draft.EventType != "file.uploaded" || draft.DataVersion != 2 || draft.ActorType != "user" || draft.ActorID != testID(5).String() {
			t.Errorf("envelope = %s v%d %s/%s", draft.EventType, draft.DataVersion, draft.ActorType, draft.ActorID)
		}
		if !draft.EventCreatedAt.Equal(now) {
			t.Errorf("event time = %v", draft.EventCreatedAt)
		}
		if draft.URL != fmt.Sprintf("https://receiver.example/%s", endpointID) {
			t.Errorf("url = %s", draft.URL)
		}
		if draft.SecretID != testID(99) {
			t.Errorf("secret id = %s, want the snapshot secret", draft.SecretID)
		}
	}
	if _, ok := store.deliveries[deliveryKey(tenant.DefaultID, testID(3), testID(11))]; ok {
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

func TestDispatcher_SubjectUserIDsIncludeActorAndExplicitSubjects(t *testing.T) {
	store := &fakeDispatchStore{
		endpoints: map[uuid.UUID][]ActiveEndpoint{
			tenant.DefaultID: {activeEndpoint(testID(1), tenant.DefaultID, "*")},
		},
	}
	dispatcher := NewDispatcher(store)
	event := testEvent(testID(14), "file.deleted", tenant.DefaultID, time.Now())
	event.SubjectUserIDs = []uuid.UUID{testID(6), testID(5)}

	created, err := dispatcher.Dispatch(context.Background(), event)
	if err != nil || created != 1 {
		t.Fatalf("Dispatch() = (%d, %v), want (1, nil)", created, err)
	}
	draft := store.deliveries[deliveryKey(tenant.DefaultID, testID(1), testID(14))]
	if len(draft.SubjectUserIDs) != 2 {
		t.Fatalf("subject ids = %v, want the actor and explicit owner once each", draft.SubjectUserIDs)
	}
	want := map[uuid.UUID]bool{testID(5): true, testID(6): true}
	for _, id := range draft.SubjectUserIDs {
		if !want[id] {
			t.Fatalf("unexpected subject id %s in %v", id, draft.SubjectUserIDs)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("missing subject ids %v", want)
	}
}

func TestDispatcher_SubjectUserIDsFallBackToPayloadForLegacyEvents(t *testing.T) {
	store := &fakeDispatchStore{
		endpoints: map[uuid.UUID][]ActiveEndpoint{
			tenant.DefaultID: {activeEndpoint(testID(1), tenant.DefaultID, "*")},
		},
	}
	dispatcher := NewDispatcher(store)
	event := testEvent(testID(15), "file.deleted", tenant.DefaultID, time.Now())
	event.SubjectUserIDs = nil
	event.Data = json.RawMessage(`{"ownerId":"` + testID(6).String() + `"}`)

	if _, err := dispatcher.Dispatch(context.Background(), event); err != nil {
		t.Fatalf("Dispatch(): %v", err)
	}
	draft := store.deliveries[deliveryKey(tenant.DefaultID, testID(1), testID(15))]
	want := map[uuid.UUID]bool{testID(5): true, testID(6): true}
	for _, id := range draft.SubjectUserIDs {
		delete(want, id)
	}
	if len(want) != 0 {
		t.Fatalf("subject ids = %v, missing %v", draft.SubjectUserIDs, want)
	}
}

// The tenant comes from the envelope, never from the job context.
func TestDispatcher_UsesEnvelopeTenant(t *testing.T) {
	otherTenant := testID(42)
	store := &fakeDispatchStore{
		endpoints: map[uuid.UUID][]ActiveEndpoint{
			otherTenant:      {activeEndpoint(testID(9), otherTenant, "*")},
			tenant.DefaultID: {activeEndpoint(testID(10), tenant.DefaultID, "*")},
		},
	}
	dispatcher := NewDispatcher(store)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	created, err := dispatcher.Dispatch(ctx, testEvent(testID(12), "user.registered", otherTenant, time.Now()))
	if err != nil || created != 1 {
		t.Fatalf("Dispatch() = (%d, %v), want (1, nil)", created, err)
	}
	if _, ok := store.deliveries[deliveryKey(otherTenant, testID(9), testID(12))]; !ok {
		t.Fatalf("deliveries = %v, want endpoint 9 of tenant 42", store.deliveries)
	}
}

func TestDispatcher_InvalidEventsArePermanent(t *testing.T) {
	dispatcher := NewDispatcher(&fakeDispatchStore{})
	valid := testEvent(testID(13), "file.uploaded", tenant.DefaultID, time.Now())

	cases := map[string]func(*events.Event){
		"no tenant":      func(e *events.Event) { e.TenantID = uuid.Nil },
		"no id":          func(e *events.Event) { e.ID = uuid.Nil },
		"no actor":       func(e *events.Event) { e.Actor = events.Actor{} },
		"non-uuid actor": func(e *events.Event) { e.Actor = events.Actor{Type: events.ActorUser, ID: "api-key-name"} },
		"pattern type":   func(e *events.Event) { e.Type = "file.*" },
		"no version":     func(e *events.Event) { e.DataVersion = 0 },
		"nil subject":    func(e *events.Event) { e.SubjectUserIDs = []uuid.UUID{uuid.Nil} },
		"duplicate subject": func(e *events.Event) {
			e.SubjectUserIDs = []uuid.UUID{testID(6), testID(6)}
		},
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
