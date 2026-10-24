package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
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
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }

	event := webhookcontracts.Event{
		ID:         "evt_1",
		Type:       "file.uploaded",
		OccurredAt: now,
		Data:       json.RawMessage(`{"fileId":7}`),
	}
	created, err := dispatcher.Dispatch(ctx, event)
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

	// Retrying the same event must be idempotent.
	created, err = dispatcher.Dispatch(ctx, event)
	if err != nil {
		t.Fatalf("second Dispatch(): %v", err)
	}
	if created != 0 {
		t.Fatalf("second Dispatch created = %d, want 0", created)
	}
}

func TestDispatcher_AssignsIDAndResolvesTenant(t *testing.T) {
	store := &fakeDispatchStore{
		endpoints: map[int64][]repository.ActiveEndpoint{
			42: {activeEndpoint(9, 42, "*")},
		},
	}
	dispatcher := NewDispatcher(store)
	dispatcher.now = func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) }
	ctx := tenant.WithID(context.Background(), 42)

	created, err := dispatcher.Dispatch(ctx, webhookcontracts.Event{Type: "user.registered"})
	if err != nil {
		t.Fatalf("Dispatch(): %v", err)
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	var draft repository.DeliveryDraft
	for _, d := range store.deliveries {
		draft = d
	}
	if draft.TenantID != 42 {
		t.Errorf("tenant = %d, want 42", draft.TenantID)
	}
	if draft.EventID == "" || draft.EventID[:4] != "evt_" {
		t.Errorf("event ID = %q, want generated evt_ prefix", draft.EventID)
	}
	if string(draft.Payload) != "{}" {
		t.Errorf("payload = %s, want {}", draft.Payload)
	}
}

func TestDispatcher_NoEndpointsAndValidation(t *testing.T) {
	dispatcher := NewDispatcher(&fakeDispatchStore{})
	if created, err := dispatcher.Dispatch(context.Background()); err != nil || created != 0 {
		t.Fatalf("empty Dispatch = (%d, %v)", created, err)
	}

	if _, err := dispatcher.Dispatch(context.Background(), webhookcontracts.Event{Type: "file.*"}); !errors.Is(err, webhookcontracts.ErrInvalidEvent) {
		t.Fatalf("pattern event error = %v, want ErrInvalidEvent", err)
	}

	store := &fakeDispatchStore{callErr: errors.New("boom")}
	dispatcher = NewDispatcher(store)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	if _, err := dispatcher.Dispatch(ctx, webhookcontracts.Event{Type: "file.uploaded"}); err == nil {
		t.Fatal("Dispatch() should surface endpoint load failures")
	}
}
