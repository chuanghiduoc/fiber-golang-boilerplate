package handler

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// ListDeliveries returns a page of the tenant's delivery history.
func (h *Handler) ListDeliveries(ctx context.Context, in *listWebhookDeliveriesInput) (*httpapi.ListOutput[dto.WebhookDeliveryResponse], error) {
	items, hasMore, err := h.service.ListDeliveries(ctx, in.EndpointID, in.Status, in.Limit, in.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := toDeliveryResponses(items)
	next := pagination.NextCursor(out, func(d dto.WebhookDeliveryResponse) (time.Time, uuid.UUID) {
		return d.CreatedAt, d.ID
	})
	return &httpapi.ListOutput[dto.WebhookDeliveryResponse]{
		Body: listEnvelope(ctx, pathWebhookDeliveries, out, hasMore, next),
	}, nil
}

// GetDelivery returns one delivery including its retained payload snapshot.
func (h *Handler) GetDelivery(ctx context.Context, in *getWebhookDeliveryInput) (*httpapi.Output[dto.WebhookDeliveryResponse], error) {
	delivery, err := h.service.GetDelivery(ctx, in.DeliveryID)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.WebhookDeliveryResponse]{Body: toDeliveryResponse(delivery, true)}, nil
}

// ListDeliveryAttempts returns every recorded attempt for one delivery.
func (h *Handler) ListDeliveryAttempts(ctx context.Context, in *listWebhookDeliveryAttemptsInput) (*httpapi.ListOutput[dto.WebhookAttemptResponse], error) {
	items, err := h.service.ListDeliveryAttempts(ctx, in.DeliveryID)
	if err != nil {
		return nil, err
	}
	return &httpapi.ListOutput[dto.WebhookAttemptResponse]{
		Body: listEnvelope(ctx, fmt.Sprintf(pathWebhookAttempts, in.DeliveryID), toAttemptResponses(items), false, ""),
	}, nil
}

// ReplayDelivery creates a new delivery of a retained payload and points
// Location at the new status resource.
func (h *Handler) ReplayDelivery(ctx context.Context, in *replayWebhookDeliveryInput) (*httpapi.Accepted[dto.WebhookDeliveryResponse], error) {
	delivery, err := h.service.ReplayDelivery(ctx, actorID(ctx), in.DeliveryID)
	if err != nil {
		return nil, err
	}
	return httpapi.NewAccepted(deliveryLocation(delivery.ID), toDeliveryResponse(delivery, true)), nil
}
