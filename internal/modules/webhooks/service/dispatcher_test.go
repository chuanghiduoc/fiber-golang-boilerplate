package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
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
	dispatcher := NewDispatcher(store, nil)
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
	dispatcher := NewDispatcher(store, nil)
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
	dispatcher := NewDispatcher(&fakeDispatchStore{}, nil)
	if created, err := dispatcher.Dispatch(context.Background()); err != nil || created != 0 {
		t.Fatalf("empty Dispatch = (%d, %v)", created, err)
	}

	if _, err := dispatcher.Dispatch(context.Background(), webhookcontracts.Event{Type: "file.*"}); !errors.Is(err, webhookcontracts.ErrInvalidEvent) {
		t.Fatalf("pattern event error = %v, want ErrInvalidEvent", err)
	}

	store := &fakeDispatchStore{callErr: errors.New("boom")}
	dispatcher = NewDispatcher(store, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	if _, err := dispatcher.Dispatch(ctx, webhookcontracts.Event{Type: "file.uploaded"}); err == nil {
		t.Fatal("Dispatch() should surface endpoint load failures")
	}
}

type fakeTxQueue struct {
	jobs []jobs.Job
}

func (f *fakeTxQueue) EnqueueOnceTx(_ context.Context, _ pgx.Tx, job jobs.Job) (id int64, created bool, err error) {
	f.jobs = append(f.jobs, job)
	return int64(len(f.jobs)), true, nil
}

// fakeTx is a non-nil pgx.Tx; the fake queue never calls it.
type fakeTx struct{ pgx.Tx }

// TestDispatcher_DispatchTxEnqueuesStableFanoutJob proves the transactional
// path enqueues one idempotent job carrying a fixed event ID, and that running
// the job (twice, as at-least-once delivery may) creates each delivery once.
func TestDispatcher_DispatchTxEnqueuesStableFanoutJob(t *testing.T) {
	store := &fakeDispatchStore{
		endpoints: map[int64][]repository.ActiveEndpoint{
			7: {activeEndpoint(1, 7, "file.*"), activeEndpoint(2, 7, "user.*")},
		},
	}
	queue := &fakeTxQueue{}
	dispatcher := NewDispatcher(store, queue)
	ctx := tenant.WithID(context.Background(), 7)

	if err := dispatcher.DispatchTx(ctx, nil, webhookcontracts.Event{Type: "file.uploaded"}); !errors.Is(err, jobs.ErrNoTx) {
		t.Fatalf("nil tx error = %v, want jobs.ErrNoTx", err)
	}
	if err := dispatcher.DispatchTx(ctx, fakeTx{}, webhookcontracts.Event{
		Type: "file.uploaded",
		Data: json.RawMessage(`{"fileId":3}`),
	}); err != nil {
		t.Fatalf("DispatchTx: %v", err)
	}
	if len(queue.jobs) != 1 {
		t.Fatalf("enqueued jobs = %d, want 1", len(queue.jobs))
	}
	job := queue.jobs[0]
	if job.Kind != DispatchJobKind || job.TenantID != 7 {
		t.Fatalf("job = %+v", job)
	}
	var payload dispatchJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.ID == "" || job.IdempotencyKey != DispatchJobKind+":"+payload.ID {
		t.Fatalf("event id %q / key %q must be stable and linked", payload.ID, job.IdempotencyKey)
	}
	if len(store.deliveries) != 0 {
		t.Fatal("DispatchTx must not create deliveries before the producer commits")
	}

	for range 2 {
		if err := dispatcher.HandleDispatchJob(context.Background(), job.Payload); err != nil {
			t.Fatalf("HandleDispatchJob: %v", err)
		}
	}
	if len(store.deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1 (only the file.* endpoint, once)", len(store.deliveries))
	}
	if _, ok := store.deliveries[fmt.Sprintf("7/1/%s", payload.ID)]; !ok {
		t.Fatalf("delivery must use the enqueued event id; got %v", store.deliveries)
	}

	if err := dispatcher.HandleDispatchJob(context.Background(), json.RawMessage(`{"type":"file.uploaded"}`)); err == nil {
		t.Fatal("a dispatch job without id/tenant must fail")
	}
}
