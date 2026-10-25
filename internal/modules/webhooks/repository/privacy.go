package repository

import (
	"context"
	"strconv"
	"time"

	webhooksprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/privacy"
	webhookssqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
)

// PrivacyStore implements webhooks/privacy.Store over sqlc.
type PrivacyStore struct {
	q *webhookssqlc.Queries
}

var _ webhooksprivacy.Store = (*PrivacyStore)(nil)

func NewPrivacyStore(db webhookssqlc.DBTX) *PrivacyStore {
	return &PrivacyStore{q: webhookssqlc.New(db)}
}

func (s *PrivacyStore) ListEvents(ctx context.Context, tenantID, userID int64) ([]webhooksprivacy.Event, error) {
	rows, err := s.q.ListUserWebhookEvents(ctx, webhookssqlc.ListUserWebhookEventsParams{
		TenantID: tenantID, UserID: strconv.FormatInt(userID, 10),
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksprivacy.Event, len(rows))
	for i, row := range rows {
		out[i] = webhooksprivacy.Event{
			ID: row.EventID, Type: row.EventType, DataVersion: row.DataVersion,
			ActorType: row.ActorType, ActorID: row.ActorID.String,
			OccurredAt: row.OccurredAt.Time, Payload: row.Payload,
			CreatedAt: row.CreatedAt.Time,
		}
	}
	return out, nil
}

func (s *PrivacyStore) ListDeliveries(ctx context.Context, tenantID, userID int64) ([]webhooksprivacy.Delivery, error) {
	rows, err := s.q.ListUserWebhookDeliveries(ctx, webhookssqlc.ListUserWebhookDeliveriesParams{
		TenantID: tenantID, UserID: strconv.FormatInt(userID, 10),
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksprivacy.Delivery, len(rows))
	for i, row := range rows {
		var statusCode *int32
		if row.LastStatusCode.Valid {
			value := row.LastStatusCode.Int32
			statusCode = &value
		}
		out[i] = webhooksprivacy.Delivery{
			ID: row.ID, EventID: row.EventID, URL: row.Url, Status: row.Status,
			Attempts: row.Attempts, MaxAttempts: row.MaxAttempts,
			LastStatusCode: statusCode, LastError: row.LastError.String,
			DeliveredAt: privacyTimePtr(row.DeliveredAt.Time, row.DeliveredAt.Valid),
			CreatedAt:   row.CreatedAt.Time,
		}
	}
	return out, nil
}

func (s *PrivacyStore) DeleteEvents(ctx context.Context, tenantID, userID int64) error {
	user := strconv.FormatInt(userID, 10)
	if err := s.q.DeleteUserWebhookDeliveries(ctx, webhookssqlc.DeleteUserWebhookDeliveriesParams{
		TenantID: tenantID, UserID: user,
	}); err != nil {
		return err
	}
	return s.q.DeleteUserWebhookEvents(ctx, webhookssqlc.DeleteUserWebhookEventsParams{
		TenantID: tenantID, UserID: user,
	})
}

func privacyTimePtr(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	return &t
}
