package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
)

// WebhookTxDispatcher is the optional transactional side of the webhooks
// dispatcher. It cannot live in the stdlib-only contracts package because it
// takes the producer's pgx.Tx.
type WebhookTxDispatcher interface {
	DispatchTx(ctx context.Context, tx pgx.Tx, events ...webhookcontracts.Event) error
}

// dispatchWebhookTx enqueues one domain event inside tx.
func dispatchWebhookTx(ctx context.Context, dispatcher WebhookTxDispatcher, tx pgx.Tx, eventType string, tenantID int64, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}
	return dispatcher.DispatchTx(ctx, tx, webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	})
}

// SetWebhookDispatcher wires outbound domain events.
func (s *userService) SetWebhookDispatcher(dispatcher webhookcontracts.Dispatcher) {
	s.webhooks = dispatcher
}

// AttachWebhookDispatcher wires the production dispatcher without widening the
// UserService interface, so handler fakes remain small.
func AttachWebhookDispatcher(svc UserService, dispatcher webhookcontracts.Dispatcher) {
	if s, ok := svc.(*userService); ok {
		s.webhooks = dispatcher
	}
}

// dispatchWebhook emits one best-effort domain event. Delivery failures are
// logged; they must not turn a committed registration into an API error.
func (s *userService) dispatchWebhook(ctx context.Context, eventType string, tenantID int64, data any) {
	if s.webhooks == nil {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Warn("failed to marshal webhook payload", slog.String("event", eventType), slog.Any("error", err))
		return
	}
	if _, err := s.webhooks.Dispatch(context.WithoutCancel(ctx), webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	}); err != nil {
		slog.Warn("failed to dispatch webhook event", slog.String("event", eventType), slog.Any("error", err))
	}
}
