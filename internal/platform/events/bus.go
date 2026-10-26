package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/metrics"
)

var (
	// ErrNoQueue is returned when a durable subscription is published without
	// a job queue.
	ErrNoQueue = errors.New("events: durable delivery requires a job queue")
	// ErrNoTx is returned when PublishTx receives a nil transaction.
	ErrNoTx = errors.New("events: transaction is nil")
	// ErrNoRegistrar is returned when RegisterJobs receives a nil registrar.
	ErrNoRegistrar = errors.New("events: job registrar is nil")
	// ErrDuplicateDurableHandler is returned when a durable job kind is
	// registered twice. A job can only carry one handler's idempotency key.
	ErrDuplicateDurableHandler = errors.New("events: durable handler already registered")
)

// Queue is the durable jobs surface used by the bus. *jobs.Queue implements it.
type Queue interface {
	EnqueueOnce(ctx context.Context, job jobs.Job) (int64, bool, error)
	EnqueueOnceTx(ctx context.Context, tx pgx.Tx, job jobs.Job) (int64, bool, error)
}

// Handler processes one event. Returning an error is reported to the
// publisher; durable delivery retries through the jobs worker.
type Handler func(context.Context, Event) error

type localSubscription struct {
	handler Handler
}

type durableSubscription struct {
	Event       string
	Handler     string
	Kind        string
	MaxAttempts int
	Fn          Handler
}

// DurableOption configures a durable subscription.
type DurableOption func(*durableSubscription)

// WithJobKind overrides the default events.<event>.<handler> job kind. Kinds
// must be unique across durable subscriptions because the idempotency key is
// scoped to the event, handler, and tenant.
func WithJobKind(kind string) DurableOption {
	return func(sub *durableSubscription) {
		sub.Kind = cleanName(kind)
	}
}

// WithMaxAttempts sets the job attempt budget. Zero keeps the queue default.
// Negative values are rejected when the subscription is registered.
func WithMaxAttempts(attempts int) DurableOption {
	return func(sub *durableSubscription) {
		sub.MaxAttempts = attempts
	}
}

// Bus dispatches events to local handlers and durably enqueues matching
// subscriptions. It is safe for concurrent use.
type Bus struct {
	queue Queue

	mu       sync.RWMutex
	local    map[string][]localSubscription
	durable  map[string][]durableSubscription
	byKind   map[string]durableSubscription
	dispatch map[string]struct{}
}

// New returns a bus. queue may be nil when only local delivery is used; a
// durable publish then fails with ErrNoQueue.
func New(queue Queue) *Bus {
	return &Bus{
		queue:    queue,
		local:    make(map[string][]localSubscription),
		durable:  make(map[string][]durableSubscription),
		byKind:   make(map[string]durableSubscription),
		dispatch: make(map[string]struct{}),
	}
}

// Subscribe registers an in-process handler for eventName. Multiple handlers
// may subscribe to the same event. A nil handler or invalid name returns an
// error.
func (b *Bus) Subscribe(eventName string, handler Handler) error {
	eventName = cleanName(eventName)
	if b == nil {
		return errors.New("events: bus is nil")
	}
	if err := validType(eventName); err != nil {
		return err
	}
	if handler == nil {
		return fmt.Errorf("%w: local handler for %q is nil", ErrInvalidEvent, eventName)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.local[eventName] = append(b.local[eventName], localSubscription{handler: handler})
	return nil
}

// SubscribeDurable registers handler as a durable subscription for eventName.
// handlerName identifies the handler in the idempotency key and defaults the
// job kind to events.<event>.<handler>. The handler is executed by the jobs
// worker after RegisterJobs is called.
func (b *Bus) SubscribeDurable(eventName, handlerName string, handler Handler, opts ...DurableOption) error {
	eventName = cleanName(eventName)
	handlerName = cleanName(handlerName)
	if b == nil {
		return errors.New("events: bus is nil")
	}
	if err := validType(eventName); err != nil {
		return err
	}
	if !namePattern.MatchString(handlerName) {
		return fmt.Errorf("%w: handler %q must match %s", ErrInvalidEvent, handlerName, namePattern)
	}
	if handler == nil {
		return fmt.Errorf("%w: durable handler %q is nil", ErrInvalidEvent, handlerName)
	}

	sub := durableSubscription{
		Event:   eventName,
		Handler: handlerName,
		Kind:    "events." + eventName + "." + handlerName,
		Fn:      handler,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&sub)
		}
	}
	if !namePattern.MatchString(sub.Kind) {
		return fmt.Errorf("%w: job kind %q must match %s", ErrInvalidEvent, sub.Kind, namePattern)
	}
	if sub.MaxAttempts < 0 {
		return fmt.Errorf("%w: max attempts must not be negative", ErrInvalidEvent)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.byKind[sub.Kind]; exists {
		return fmt.Errorf("%w: kind %q", ErrDuplicateDurableHandler, sub.Kind)
	}
	for _, existing := range b.durable[eventName] {
		if existing.Handler == handlerName {
			return fmt.Errorf("%w: %s/%s", ErrDuplicateDurableHandler, eventName, handlerName)
		}
	}
	b.durable[eventName] = append(b.durable[eventName], sub)
	b.byKind[sub.Kind] = sub
	return nil
}

// RegisterJobs registers one jobs.Handler per durable subscription. It is
// safe to call more than once; already registered kinds are skipped.
func (b *Bus) RegisterJobs(reg jobs.Registrar) error {
	if b == nil || reg == nil {
		return ErrNoRegistrar
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for kind, sub := range b.byKind {
		if _, done := b.dispatch[kind]; done {
			continue
		}
		reg.Register(kind, b.jobHandler(sub))
		b.dispatch[kind] = struct{}{}
	}
	return nil
}

// PublishLocal runs local handlers synchronously. Durable subscriptions are
// ignored. Panics are recovered per handler and combined with errors.Join.
func (b *Bus) PublishLocal(ctx context.Context, event Event) error {
	event, err := b.prepare(ctx, event)
	if err != nil {
		return err
	}
	metrics.EventsPublishedTotal.WithLabelValues(event.Type, "local").Inc()
	return b.publishLocal(ctx, event)
}

// Publish runs local handlers and enqueues durable subscriptions. All local
// handlers run before durable delivery, and every error (including handler
// panics and enqueue failures) is joined. A failed local handler does not stop
// durable delivery.
func (b *Bus) Publish(ctx context.Context, event Event) error {
	event, err := b.prepare(ctx, event)
	if err != nil {
		return err
	}
	metrics.EventsPublishedTotal.WithLabelValues(event.Type, "durable").Inc()

	localErr := b.publishLocal(ctx, event)
	durableErr := b.enqueueDurable(ctx, nil, event)
	return errors.Join(localErr, durableErr)
}

// PublishTx enqueues every durable subscription job using the caller's
// transaction, so an event produced inside a business transaction commits or
// rolls back atomically. Local subscribers are deliberately not notified:
// there is no commit hook on a pgx.Tx, so delivering inside the transaction
// would let a rollback leave a ghost event and would let a local handler error
// roll back the business work. Use Publish or PublishLocal after the commit
// when local delivery is needed.
//
// The published-events counter moves only after every enqueue succeeds. A
// later rollback of the caller's transaction can still over-count, because
// Prometheus counters cannot be rolled back; the metric means "enqueued in a
// transaction", not "committed".
func (b *Bus) PublishTx(ctx context.Context, tx pgx.Tx, event Event) error {
	if tx == nil {
		return ErrNoTx
	}
	event, err := b.prepare(ctx, event)
	if err != nil {
		return err
	}
	if err := b.enqueueDurable(ctx, tx, event); err != nil {
		return err
	}
	metrics.EventsPublishedTotal.WithLabelValues(event.Type, "durable").Inc()
	return nil
}

func (b *Bus) prepare(ctx context.Context, event Event) (Event, error) {
	if b == nil {
		return Event{}, errors.New("events: bus is nil")
	}
	if event.ID == "" {
		id, err := newID()
		if err != nil {
			return Event{}, err
		}
		event.ID = id
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	} else {
		event.OccurredAt = event.OccurredAt.UTC()
	}
	if event.TenantID == 0 {
		event.TenantID = tenant.EffectiveID(ctx)
	}
	if event.RequestID == "" {
		event.RequestID = requestctx.RequestID(ctx)
	}
	if event.Actor.Type == "" {
		event.Actor = actorFromContext(ctx)
	}
	if len(event.Data) == 0 {
		event.Data = json.RawMessage(`{}`)
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

// actorFromContext attributes an event to the request's authenticated user,
// or to the system when there is none (jobs, schedules, maintenance).
func actorFromContext(ctx context.Context) Actor {
	if id := requestctx.ActorID(ctx); id > 0 {
		return UserActor(id)
	}
	return Actor{Type: ActorSystem}
}

// validType checks an event type at subscription time.
func validType(eventType string) error {
	if !typePattern.MatchString(eventType) {
		return fmt.Errorf("%w: type %q must match %s", ErrInvalidEvent, eventType, typePattern)
	}
	return nil
}

func (b *Bus) publishLocal(ctx context.Context, event Event) error {
	b.mu.RLock()
	subscriptions := append([]localSubscription(nil), b.local[event.Type]...)
	b.mu.RUnlock()

	var errs []error
	for _, sub := range subscriptions {
		if err := invoke(ctx, sub.handler, event, "local"); err != nil {
			errs = append(errs, fmt.Errorf("events: local handler for %q: %w", event.Type, err))
		}
	}
	return errors.Join(errs...)
}

func (b *Bus) enqueueDurable(ctx context.Context, tx pgx.Tx, event Event) error {
	b.mu.RLock()
	subscriptions := append([]durableSubscription(nil), b.durable[event.Type]...)
	b.mu.RUnlock()
	if len(subscriptions) == 0 {
		return nil
	}
	if b.queue == nil {
		return ErrNoQueue
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("events: marshal %q for durable delivery: %w", event.Type, err)
	}

	var errs []error
	for _, sub := range subscriptions {
		job := jobs.Job{
			TenantID:       event.TenantID,
			Kind:           sub.Kind,
			Payload:        payload,
			MaxAttempts:    sub.MaxAttempts,
			IdempotencyKey: durableIdempotencyKey(event, sub),
		}
		if tx != nil {
			_, _, err = b.queue.EnqueueOnceTx(ctx, tx, job)
		} else {
			_, _, err = b.queue.EnqueueOnce(ctx, job)
		}
		if err != nil {
			metrics.EventsEnqueueErrorsTotal.WithLabelValues(event.Type).Inc()
			errs = append(errs, fmt.Errorf("events: enqueue %q for %q: %w", sub.Kind, event.Type, err))
		}
	}
	return errors.Join(errs...)
}

func durableIdempotencyKey(event Event, sub durableSubscription) string {
	return fmt.Sprintf("event:%s:%s:%s:%d", event.Type, event.ID, sub.Handler, event.TenantID)
}

func (b *Bus) jobHandler(sub durableSubscription) jobs.Handler {
	return func(ctx context.Context, payload json.RawMessage) error {
		// A payload that does not decode, validate, or match the subscription
		// can never succeed, so it dead-letters without spending retries.
		var event Event
		if err := json.Unmarshal(payload, &event); err != nil {
			return jobs.Permanent(fmt.Errorf("events: decode durable %q payload: %w", sub.Kind, err))
		}
		if err := event.Validate(); err != nil {
			return jobs.Permanent(err)
		}
		if event.Type != sub.Event {
			return jobs.Permanent(fmt.Errorf("events: durable %q payload has event %q, want %q", sub.Kind, event.Type, sub.Event))
		}

		if event.TenantID > 0 {
			ctx = tenant.WithID(ctx, event.TenantID)
		}
		ctx = requestctx.WithRequestID(ctx, event.RequestID)
		if actorID := event.Actor.userID(); actorID > 0 {
			ctx = requestctx.WithActorID(ctx, actorID)
		}
		if err := invoke(ctx, sub.Fn, event, sub.Handler); err != nil {
			return fmt.Errorf("events: durable handler %q for %q: %w", sub.Handler, sub.Event, err)
		}
		return nil
	}
}

func invoke(ctx context.Context, handler Handler, event Event, name string) (err error) {
	outcome := "success"
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = "panic"
			err = fmt.Errorf("events: handler %q panic: %v\n%s", name, recovered, debug.Stack())
		}
		metrics.EventsHandledTotal.WithLabelValues(event.Type, outcome).Inc()
	}()

	err = handler(ctx, event)
	if err != nil {
		outcome = "error"
	}
	return err
}
