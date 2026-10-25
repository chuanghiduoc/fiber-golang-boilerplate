package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

type fakeTx struct{ pgx.Tx }

type fakeQueue struct {
	mu       sync.Mutex
	jobs     []jobs.Job
	txCalls  int
	failWith error
}

func (q *fakeQueue) EnqueueOnce(_ context.Context, job jobs.Job) (id int64, created bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, job)
	if q.failWith != nil {
		return 0, false, q.failWith
	}
	return int64(len(q.jobs)), true, nil
}

func (q *fakeQueue) EnqueueOnceTx(_ context.Context, tx pgx.Tx, job jobs.Job) (id int64, created bool, err error) {
	q.mu.Lock()
	q.txCalls++
	q.mu.Unlock()
	if tx == nil {
		return 0, false, errors.New("nil transaction")
	}
	return q.EnqueueOnce(context.Background(), job)
}

func (q *fakeQueue) snapshot() []jobs.Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]jobs.Job(nil), q.jobs...)
}

type fakeRegistrar struct {
	handlers map[string]jobs.Handler
}

func (r *fakeRegistrar) Register(kind string, handler jobs.Handler) {
	if r.handlers == nil {
		r.handlers = make(map[string]jobs.Handler)
	}
	r.handlers[kind] = handler
}

func TestPublishLocalPropagatesMetadata(t *testing.T) {
	bus := New(nil)
	require.NoError(t, bus.Subscribe("user.created", func(ctx context.Context, event Event) error {
		assert.Equal(t, "user.created", event.Type)
		assert.NotEmpty(t, event.ID)
		assert.Equal(t, int64(7), event.TenantID)
		assert.Equal(t, "req-1", event.RequestID)
		assert.Equal(t, UserActor(42), event.Actor)
		assert.Equal(t, 1, event.DataVersion)
		assert.False(t, event.OccurredAt.IsZero())

		var data struct {
			UserID int64 `json:"userId"`
		}
		require.NoError(t, json.Unmarshal(event.Data, &data))
		assert.Equal(t, int64(9), data.UserID)
		return nil
	}))

	ctx := requestctx.WithActorID(
		requestctx.WithRequestID(tenant.WithID(context.Background(), 7), "req-1"),
		42,
	)
	event, err := NewEvent("user.created", 1, map[string]any{"userId": 9})
	require.NoError(t, err)
	require.NoError(t, bus.PublishLocal(ctx, event))
}

func TestPublishLocalKeepsExplicitMetadata(t *testing.T) {
	bus := New(nil)
	require.NoError(t, bus.Subscribe("explicit", func(_ context.Context, event Event) error {
		assert.Equal(t, "chosen-id", event.ID)
		assert.Equal(t, int64(3), event.TenantID)
		assert.Equal(t, "chosen-request", event.RequestID)
		assert.Equal(t, Actor{Type: ActorService, ID: "key_1"}, event.Actor)
		return nil
	}))

	ctx := requestctx.WithActorID(
		requestctx.WithRequestID(tenant.WithID(context.Background(), 7), "ctx-request"),
		1,
	)
	err := bus.PublishLocal(ctx, Event{
		ID:          "chosen-id",
		Type:        "explicit",
		DataVersion: 2,
		TenantID:    3,
		RequestID:   "chosen-request",
		Actor:       Actor{Type: ActorService, ID: "key_1"},
		OccurredAt:  time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC),
		Data:        json.RawMessage(`{"ok":true}`),
	})
	require.NoError(t, err)
}

func TestPublishLocalIsolatesPanicsAndJoinsErrors(t *testing.T) {
	bus := New(nil)
	sentinel := errors.New("handler failed")
	var calls int

	require.NoError(t, bus.Subscribe("audit", func(context.Context, Event) error {
		calls++
		panic("boom")
	}))
	require.NoError(t, bus.Subscribe("audit", func(context.Context, Event) error {
		calls++
		return sentinel
	}))
	require.NoError(t, bus.Subscribe("audit", func(context.Context, Event) error {
		calls++
		return nil
	}))

	event, err := NewEvent("audit", 1, nil)
	require.NoError(t, err)
	err = bus.PublishLocal(context.Background(), event)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.ErrorContains(t, err, "panic")
	assert.Equal(t, 3, calls)
}

func TestPublishDurableEnqueuesOneJobPerHandler(t *testing.T) {
	queue := &fakeQueue{}
	bus := New(queue)

	localCalls := 0
	require.NoError(t, bus.Subscribe("invoice.paid", func(context.Context, Event) error {
		localCalls++
		return nil
	}))

	var durableCalls int
	durable := func(context.Context, Event) error {
		durableCalls++
		return nil
	}
	require.NoError(t, bus.SubscribeDurable("invoice.paid", "email", durable, WithMaxAttempts(4)))
	require.NoError(t, bus.SubscribeDurable("invoice.paid", "ledger", durable, WithJobKind("ledger.invoice.paid")))

	ctx := tenant.WithID(context.Background(), 5)
	event, err := NewEvent("invoice.paid", 1, map[string]any{"invoiceId": 12})
	require.NoError(t, err)
	require.NoError(t, bus.Publish(ctx, event))
	assert.Equal(t, 1, localCalls)
	assert.Equal(t, 0, durableCalls, "durable handlers run in the worker, not during Publish")

	enqueued := queue.snapshot()
	require.Len(t, enqueued, 2)
	byKind := make(map[string]jobs.Job, len(enqueued))
	for _, job := range enqueued {
		byKind[job.Kind] = job
		assert.Equal(t, int64(5), job.TenantID)
		assert.Equal(t, "event:invoice.paid:"+event.ID+":"+handlerNameForKind(job.Kind)+":5", job.IdempotencyKey)
		var decoded Event
		require.NoError(t, json.Unmarshal(job.Payload, &decoded))
		assert.Equal(t, event.ID, decoded.ID)
		assert.Equal(t, int64(5), decoded.TenantID)
	}
	assert.Contains(t, byKind, "events.invoice.paid.email")
	assert.Contains(t, byKind, "ledger.invoice.paid")
	assert.Equal(t, 4, byKind["events.invoice.paid.email"].MaxAttempts)
}

func handlerNameForKind(kind string) string {
	switch kind {
	case "events.invoice.paid.email":
		return "email"
	case "ledger.invoice.paid":
		return "ledger"
	default:
		return ""
	}
}

func TestPublishDurableQueueErrorIsJoinedWithLocalError(t *testing.T) {
	sentinel := errors.New("queue down")
	queue := &fakeQueue{failWith: sentinel}
	bus := New(queue)

	require.NoError(t, bus.SubscribeDurable("order.created", "email", func(context.Context, Event) error { return nil }))
	localRan := false
	require.NoError(t, bus.Subscribe("order.created", func(context.Context, Event) error {
		localRan = true
		return nil
	}))

	event, err := NewEvent("order.created", 1, nil)
	require.NoError(t, err)
	err = bus.Publish(tenant.WithID(context.Background(), 2), event)
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.True(t, localRan)
}

func TestPublishRequiresQueueForDurableSubscription(t *testing.T) {
	bus := New(nil)
	require.NoError(t, bus.SubscribeDurable("user.deleted", "cleanup", func(context.Context, Event) error { return nil }))

	event, err := NewEvent("user.deleted", 1, nil)
	require.NoError(t, err)
	assert.ErrorIs(t, bus.Publish(context.Background(), event), ErrNoQueue)
}

func TestPublishLocalSkipsDurableQueue(t *testing.T) {
	queue := &fakeQueue{}
	bus := New(queue)
	require.NoError(t, bus.SubscribeDurable("user.deleted", "cleanup", func(context.Context, Event) error { return nil }))

	event, err := NewEvent("user.deleted", 1, nil)
	require.NoError(t, err)
	require.NoError(t, bus.PublishLocal(context.Background(), event))
	assert.Empty(t, queue.snapshot(), "PublishLocal must never enqueue durable work")
}

func TestPublishTxUsesTransaction(t *testing.T) {
	queue := &fakeQueue{}
	bus := New(queue)
	require.NoError(t, bus.SubscribeDurable("tenant.created", "provision", func(context.Context, Event) error { return nil }))

	event, err := NewEvent("tenant.created", 1, nil)
	require.NoError(t, err)
	require.NoError(t, bus.PublishTx(context.Background(), &fakeTx{}, event))

	queue.mu.Lock()
	defer queue.mu.Unlock()
	assert.Equal(t, 1, queue.txCalls)
	assert.Len(t, queue.jobs, 1)
}

func TestPublishTxRejectsNilTransaction(t *testing.T) {
	bus := New(&fakeQueue{})
	event, err := NewEvent("tenant.created", 1, nil)
	require.NoError(t, err)
	assert.ErrorIs(t, bus.PublishTx(context.Background(), nil, event), ErrNoTx)
}

func TestSubscribeDurableValidation(t *testing.T) {
	bus := New(&fakeQueue{})
	handler := func(context.Context, Event) error { return nil }

	require.Error(t, bus.Subscribe("bad name", handler))
	require.Error(t, bus.Subscribe("ok", nil))
	require.Error(t, bus.SubscribeDurable("bad name", "handler", handler))
	require.Error(t, bus.SubscribeDurable("ok", "bad name", handler))
	require.Error(t, bus.SubscribeDurable("ok", "handler", nil))
	require.Error(t, bus.SubscribeDurable("ok", "handler", handler, WithMaxAttempts(-1)))

	require.NoError(t, bus.SubscribeDurable("ok", "handler", handler))
	require.ErrorIs(t, bus.SubscribeDurable("ok", "handler", handler), ErrDuplicateDurableHandler)
	require.ErrorIs(t, bus.SubscribeDurable("other", "other", handler, WithJobKind("events.ok.handler")), ErrDuplicateDurableHandler)
}

func TestRegisterJobsDispatchesEventWithContext(t *testing.T) {
	bus := New(&fakeQueue{})
	received := make(chan Event, 1)
	require.NoError(t, bus.SubscribeDurable("user.created", "welcome", func(ctx context.Context, event Event) error {
		assert.Equal(t, int64(11), tenant.IDFromContext(ctx))
		assert.Equal(t, "req-9", requestctx.RequestID(ctx))
		assert.Equal(t, int64(77), requestctx.ActorID(ctx))
		received <- event
		return nil
	}))

	registrar := &fakeRegistrar{}
	require.NoError(t, bus.RegisterJobs(registrar))
	require.Len(t, registrar.handlers, 1)
	handler := registrar.handlers["events.user.created.welcome"]
	require.NotNil(t, handler)

	event, err := NewEvent("user.created", 1, map[string]any{"userId": 1})
	require.NoError(t, err)
	event.TenantID = 11
	event.RequestID = "req-9"
	event.Actor = UserActor(77)
	payload, err := json.Marshal(event)
	require.NoError(t, err)

	require.NoError(t, handler(context.Background(), payload))
	select {
	case got := <-received:
		assert.Equal(t, event.ID, got.ID)
	default:
		t.Fatal("durable handler was not invoked")
	}

	// A repeated registration must not replace or duplicate handlers.
	second := &fakeRegistrar{}
	require.NoError(t, bus.RegisterJobs(second))
	assert.Empty(t, second.handlers)
}

func TestRegisterJobsRejectsWrongEventAndBadPayload(t *testing.T) {
	bus := New(&fakeQueue{})
	require.NoError(t, bus.SubscribeDurable("user.created", "welcome", func(context.Context, Event) error { return nil }))

	registrar := &fakeRegistrar{}
	require.NoError(t, bus.RegisterJobs(registrar))
	handler := registrar.handlers["events.user.created.welcome"]

	require.Error(t, handler(context.Background(), json.RawMessage(`not-json`)))

	other, err := NewEvent("user.deleted", 1, nil)
	require.NoError(t, err)
	payload, err := json.Marshal(other)
	require.NoError(t, err)
	err = handler(context.Background(), payload)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "want"))
}

func TestNewEventValidation(t *testing.T) {
	_, err := NewEvent("bad name", 1, nil)
	require.ErrorIs(t, err, ErrInvalidEvent)

	event, err := NewEvent("valid.event", 1, json.RawMessage(`{"a":1}`))
	require.NoError(t, err)
	decoded, err := Decode[map[string]int](event)
	require.NoError(t, err)
	assert.Equal(t, 1, decoded["a"])

	event.Data = json.RawMessage(`{`)
	require.ErrorIs(t, event.Validate(), ErrInvalidEvent)

	empty, err := NewEvent("valid.event", 1, nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(empty.Data))
}

func TestEnvelopeShapeAndDefaults(t *testing.T) {
	bus := New(nil)
	var got Event
	require.NoError(t, bus.Subscribe("file.uploaded", func(_ context.Context, event Event) error {
		got = event
		return nil
	}))
	// No id and no request actor: the bus assigns a UUIDv7 and the system actor.
	require.NoError(t, bus.PublishLocal(tenant.WithID(context.Background(), 4), Event{
		Type:        "file.uploaded",
		DataVersion: 1,
		Data:        json.RawMessage(`{"id":1}`),
	}))
	id, err := uuid.Parse(got.ID)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(7), id.Version())
	assert.Equal(t, Actor{Type: ActorSystem}, got.Actor)

	got.RequestID = ""
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	assert.ElementsMatch(t, []string{"id", "type", "dataVersion", "occurredAt", "tenantId", "actor", "data"}, keys)
	assert.JSONEq(t, `{"type":"system"}`, string(fields["actor"]))
}

func TestEnvelopeValidation(t *testing.T) {
	_, err := NewEvent("file.uploaded", 0, nil)
	require.ErrorIs(t, err, ErrInvalidEvent, "dataVersion must be >= 1")
	_, err = NewEvent("File.Uploaded", 1, nil)
	require.ErrorIs(t, err, ErrInvalidEvent, "types are lowercase")

	valid, err := NewEvent("file.uploaded", 1, nil)
	require.NoError(t, err)
	for name, actor := range map[string]Actor{
		"unknown type":     {Type: "admin", ID: "1"},
		"user without id":  {Type: ActorUser},
		"system with id":   {Type: ActorSystem, ID: "1"},
		"id without type":  {ID: "1"},
		"service too long": {Type: ActorService, ID: strings.Repeat("a", maxActorIDLen+1)},
	} {
		event := valid
		event.Actor = actor
		assert.ErrorIs(t, event.Validate(), ErrInvalidEvent, name)
	}
}
