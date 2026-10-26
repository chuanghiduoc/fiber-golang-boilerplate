package service

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func (s *service) ListDeliveries(ctx context.Context, endpointID int64, status string, limit int, startingAfter string) ([]Delivery, bool, error) {
	if status != "" && status != DeliveryPending && status != DeliverySucceeded && status != DeliveryDead {
		return nil, false, apperror.NewBadRequest("status must be one of: pending, succeeded, dead")
	}
	if endpointID < 0 {
		return nil, false, apperror.NewBadRequest("invalid endpointId")
	}
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	deliveries, err := s.deliveries.ListDeliveries(ctx, ListDeliveriesParams{
		EndpointID: endpointID,
		Status:     status,
		Cursor:     cur,
	})
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list webhook deliveries")
	}
	hasMore := len(deliveries) > pageSize
	if hasMore {
		deliveries = deliveries[:pageSize]
	}
	return deliveries, hasMore, nil
}

func (s *service) GetDelivery(ctx context.Context, id int64) (*Delivery, error) {
	delivery, err := s.deliveries.GetDelivery(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook delivery not found")
		}
		return nil, apperror.NewInternal("failed to get webhook delivery")
	}
	return delivery, nil
}

func (s *service) ListDeliveryAttempts(ctx context.Context, id int64) ([]DeliveryAttempt, error) {
	if _, err := s.deliveries.GetDelivery(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook delivery not found")
		}
		return nil, apperror.NewInternal("failed to get webhook delivery")
	}
	attempts, err := s.deliveries.ListDeliveryAttempts(ctx, id)
	if err != nil {
		return nil, apperror.NewInternal("failed to list webhook delivery attempts")
	}
	return attempts, nil
}

// ReplayDelivery creates a new delivery of the original payload to the
// endpoint's current URL and active secret, linked to the original.
func (s *service) ReplayDelivery(ctx context.Context, actorID, id int64) (*Delivery, error) {
	original, err := s.deliveries.GetDelivery(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook delivery not found")
		}
		return nil, apperror.NewInternal("failed to get webhook delivery")
	}
	endpoint, err := s.endpoints.GetEndpoint(ctx, original.EndpointID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook endpoint not found")
		}
		return nil, apperror.NewInternal("failed to get webhook endpoint")
	}
	if endpoint.Status != StatusActive {
		return nil, apperror.NewConflict("webhook endpoint is disabled")
	}
	ciphertext, err := s.endpoints.ActiveEndpointSecret(ctx, endpoint.ID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewConflict("webhook endpoint has no active secret")
		}
		return nil, apperror.NewInternal("failed to get webhook secret")
	}

	// A replay is a new delivery (and attempt counter) of the SAME message:
	// the original event id stays the webhook-id, so a receiver deduplicates
	// it per the Standard Webhooks contract. The original rows are untouched.
	newID, created, err := s.deliveries.CreateDeliveryWithJob(ctx, DeliveryDraft{
		TenantID:         tenant.EffectiveID(ctx),
		EndpointID:       endpoint.ID,
		EventID:          original.EventID,
		EventType:        original.EventType,
		DataVersion:      original.DataVersion,
		ActorType:        original.ActorType,
		ActorID:          original.ActorID,
		EventCreatedAt:   original.OccurredAt,
		URL:              endpoint.URL,
		Payload:          original.Payload,
		SecretCiphertext: ciphertext,
		MaxAttempts:      original.MaxAttempts,
		TimeoutMS:        endpoint.TimeoutMS,
		RunAt:            s.now(),
		ReplayedFromID:   original.ID,
		IsReplay:         true,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to replay webhook delivery")
	}
	if !created {
		return nil, apperror.NewConflict("webhook delivery replay already exists")
	}
	delivery, err := s.deliveries.GetDelivery(ctx, newID)
	if err != nil {
		return nil, apperror.NewInternal("failed to load replayed webhook delivery")
	}
	s.recordAudit(ctx, actorID, "webhook.delivery.replayed", "webhook_delivery", newID, map[string]any{
		"originalDeliveryId": original.ID,
		"endpointId":         endpoint.ID,
		"eventType":          original.EventType,
	})
	return delivery, nil
}
