package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	webhooksprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/privacy"
	webhookssqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// PrivacyStore implements webhooks/privacy.Store over sqlc.
type PrivacyStore struct {
	q  *webhookssqlc.Queries
	tx *database.TxManager
}

var _ webhooksprivacy.Store = (*PrivacyStore)(nil)

func NewPrivacyStore(db webhookssqlc.DBTX, tx *database.TxManager) *PrivacyStore {
	return &PrivacyStore{q: webhookssqlc.New(db), tx: tx}
}

func (s *PrivacyStore) ListEvents(ctx context.Context, tenantID, userID uuid.UUID, cursor pagination.DBCursor) ([]webhooksprivacy.Event, error) {
	rows, err := s.q.ListUserWebhookEvents(ctx, webhookssqlc.ListUserWebhookEventsParams{
		TenantID:         tenantID,
		UserID:           userID,
		CursorOccurredAt: cursor.CreatedAtParam(),
		CursorID:         cursor.IDParam(),
		RowLimit:         cursor.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]webhooksprivacy.Event, len(rows))
	for i, row := range rows {
		out[i] = webhooksprivacy.Event{
			ID: row.EventID, Type: row.EventType, DataVersion: row.DataVersion,
			ActorType: row.ActorType, ActorID: uuidString(row.ActorID),
			OccurredAt: row.OccurredAt.Time, Payload: row.Payload,
			CreatedAt: row.CreatedAt.Time,
		}
	}
	return out, nil
}

func (s *PrivacyStore) ListDeliveries(ctx context.Context, tenantID, userID uuid.UUID, cursor pagination.DBCursor) ([]webhooksprivacy.Delivery, error) {
	rows, err := s.q.ListUserWebhookDeliveries(ctx, webhookssqlc.ListUserWebhookDeliveriesParams{
		TenantID:        tenantID,
		UserID:          userID,
		CursorCreatedAt: cursor.CreatedAtParam(),
		CursorID:        cursor.IDParam(),
		RowLimit:        cursor.Limit,
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

func (s *PrivacyStore) DeleteEvents(ctx context.Context, tenantID, userID uuid.UUID) error {
	if s == nil || s.tx == nil {
		return ErrNoPool
	}
	return s.tx.WithTx(ctx, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.DeleteUserWebhookDeliveries(ctx, webhookssqlc.DeleteUserWebhookDeliveriesParams{
			TenantID: tenantID, UserID: userID,
		}); err != nil {
			return err
		}
		return q.DeleteUserWebhookEvents(ctx, webhookssqlc.DeleteUserWebhookEventsParams{
			TenantID: tenantID, UserID: userID,
		})
	})
}

func privacyTimePtr(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	return &t
}
