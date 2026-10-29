package repository

import (
	"context"
	"time"

	"github.com/google/uuid"

	notificationsprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/privacy"
	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
)

// PrivacyStore implements notifications/privacy.Store over sqlc.
type PrivacyStore struct {
	q *notificationsqlc.Queries
}

var _ notificationsprivacy.Store = (*PrivacyStore)(nil)

func NewPrivacyStore(db notificationsqlc.DBTX) *PrivacyStore {
	return &PrivacyStore{q: notificationsqlc.New(db)}
}

func (s *PrivacyStore) ListNotifications(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]notificationsprivacy.Notification, bool, error) {
	if limit <= 0 {
		return []notificationsprivacy.Notification{}, false, nil
	}
	rows, err := s.q.ListNotificationsForPrivacy(ctx, notificationsqlc.ListNotificationsForPrivacyParams{
		TenantID: tenantID, UserID: userID, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > int(limit)
	if truncated {
		rows = rows[:limit]
	}
	out := make([]notificationsprivacy.Notification, len(rows))
	for i, row := range rows {
		out[i] = notificationsprivacy.Notification{
			ID: row.ID, Type: row.Type, Title: row.Title, Body: row.Body,
			Data: row.Data, ReadAt: timestampPtr(row.ReadAt.Time, row.ReadAt.Valid),
			CreatedAt: row.CreatedAt.Time,
		}
	}
	return out, truncated, nil
}

func (s *PrivacyStore) ListPreferences(ctx context.Context, tenantID, userID uuid.UUID, limit int32) ([]notificationsprivacy.Preference, bool, error) {
	if limit <= 0 {
		return []notificationsprivacy.Preference{}, false, nil
	}
	rows, err := s.q.ListPreferencesForPrivacy(ctx, notificationsqlc.ListPreferencesForPrivacyParams{
		TenantID: tenantID, UserID: userID, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > int(limit)
	if truncated {
		rows = rows[:limit]
	}
	out := make([]notificationsprivacy.Preference, len(rows))
	for i, row := range rows {
		out[i] = notificationsprivacy.Preference{
			Type: row.Type, Channel: row.Channel, Enabled: row.Enabled, UpdatedAt: row.UpdatedAt.Time,
		}
	}
	return out, truncated, nil
}

func (s *PrivacyStore) DeleteAll(ctx context.Context, tenantID, userID uuid.UUID) error {
	return s.q.DeleteAllNotificationPrivacy(ctx, notificationsqlc.DeleteAllNotificationPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
}

func timestampPtr(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	return &t
}
