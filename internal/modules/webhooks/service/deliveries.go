package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func (s *service) ListDeliveries(ctx context.Context, endpointID int64, status string, limit int, startingAfter string) ([]dto.DeliveryResponse, bool, error) {
	if status != "" && status != dto.DeliveryPending && status != dto.DeliverySucceeded && status != dto.DeliveryDead {
		return nil, false, apperror.NewBadRequest("status must be one of: pending, succeeded, dead")
	}
	if endpointID < 0 {
		return nil, false, apperror.NewBadRequest("invalid endpointId")
	}
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	deliveries, err := s.deliveries.ListDeliveries(ctx, repository.ListDeliveriesParams{
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
	out := make([]dto.DeliveryResponse, len(deliveries))
	for i := range deliveries {
		out[i] = toDeliveryResponse(&deliveries[i], false)
	}
	return out, hasMore, nil
}

func (s *service) GetDelivery(ctx context.Context, id int64) (*dto.DeliveryResponse, error) {
	delivery, err := s.deliveries.GetDelivery(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("webhook delivery not found")
		}
		return nil, apperror.NewInternal("failed to get webhook delivery")
	}
	resp := toDeliveryResponse(delivery, true)
	return &resp, nil
}

func (s *service) ListDeliveryAttempts(ctx context.Context, id int64) ([]dto.DeliveryAttemptResponse, error) {
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
	out := make([]dto.DeliveryAttemptResponse, len(attempts))
	for i := range attempts {
		out[i] = toAttemptResponse(&attempts[i])
	}
	return out, nil
}

// ReplayDelivery creates a new delivery of the original payload to the
// endpoint's current URL and active secret, linked to the original.
func (s *service) ReplayDelivery(ctx context.Context, actorID, id int64) (*dto.DeliveryResponse, error) {
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
	if endpoint.Status != dto.StatusActive {
		return nil, apperror.NewConflict("webhook endpoint is disabled")
	}
	ciphertext, err := s.endpoints.ActiveEndpointSecret(ctx, endpoint.ID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewConflict("webhook endpoint has no active secret")
		}
		return nil, apperror.NewInternal("failed to get webhook secret")
	}

	eventID := fmt.Sprintf("replay.%d.%s", original.ID, randomHex(8))
	newID, created, err := s.deliveries.CreateDeliveryWithJob(ctx, repository.DeliveryDraft{
		TenantID:         tenant.EffectiveID(ctx),
		EndpointID:       endpoint.ID,
		EventID:          eventID,
		EventType:        original.EventType,
		EventCreatedAt:   s.now(),
		URL:              endpoint.Url,
		Payload:          json.RawMessage(original.Payload),
		SecretCiphertext: ciphertext,
		MaxAttempts:      original.MaxAttempts,
		TimeoutMS:        endpoint.TimeoutMs,
		RunAt:            s.now(),
		ReplayedFromID:   original.ID,
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
	resp := toDeliveryResponse(delivery, true)
	return &resp, nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failure is unrecoverable for delivery identity; fall back
		// to a time-based suffix rather than panicking inside a request.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func toDeliveryResponse(delivery *repository.Delivery, includePayload bool) dto.DeliveryResponse {
	resp := dto.DeliveryResponse{
		ID:            delivery.ID,
		TenantID:      delivery.TenantID,
		EndpointID:    delivery.EndpointID,
		EventID:       delivery.EventID,
		EventType:     delivery.EventType,
		Status:        delivery.Status,
		Attempts:      delivery.Attempts,
		MaxAttempts:   delivery.MaxAttempts,
		NextAttemptAt: delivery.NextAttemptAt.Time.UTC(),
		CreatedAt:     delivery.CreatedAt.Time.UTC(),
		UpdatedAt:     delivery.UpdatedAt.Time.UTC(),
	}
	if delivery.LastStatusCode.Valid {
		code := int(delivery.LastStatusCode.Int32)
		resp.LastStatusCode = &code
	}
	if delivery.LastError.Valid {
		resp.LastError = delivery.LastError.String
	}
	if delivery.DeliveredAt.Valid {
		at := delivery.DeliveredAt.Time.UTC()
		resp.DeliveredAt = &at
	}
	if delivery.ReplayedFromID.Valid {
		id := delivery.ReplayedFromID.Int64
		resp.ReplayedFromID = &id
	}
	if includePayload {
		resp.Payload = json.RawMessage(delivery.Payload)
	}
	return resp
}

func toAttemptResponse(attempt *webhooksqlc.WebhookDeliveryAttempt) dto.DeliveryAttemptResponse {
	resp := dto.DeliveryAttemptResponse{
		ID:         attempt.ID,
		DeliveryID: attempt.DeliveryID,
		Attempt:    attempt.Attempt,
		DurationMS: attempt.DurationMs,
		CreatedAt:  attempt.CreatedAt.Time.UTC(),
	}
	if attempt.StatusCode.Valid {
		code := int(attempt.StatusCode.Int32)
		resp.StatusCode = &code
	}
	if attempt.Error.Valid {
		resp.Error = attempt.Error.String
	}
	return resp
}
