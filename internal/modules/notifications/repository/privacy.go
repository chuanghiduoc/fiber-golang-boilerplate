package repository

import (
	"context"
	"time"

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

func (s *PrivacyStore) ListNotifications(ctx context.Context, tenantID, userID int64) ([]notificationsprivacy.Notification, error) {
	rows, err := s.q.ListNotificationsForPrivacy(ctx, notificationsqlc.ListNotificationsForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]notificationsprivacy.Notification, len(rows))
	for i, row := range rows {
		out[i] = notificationsprivacy.Notification{
			ID: row.ID, Type: row.Type, Title: row.Title, Body: row.Body,
			Data: row.Data, ReadAt: timestampPtr(row.ReadAt.Time, row.ReadAt.Valid),
			CreatedAt: row.CreatedAt.Time,
		}
	}
	return out, nil
}

func (s *PrivacyStore) ListPreferences(ctx context.Context, tenantID, userID int64) ([]notificationsprivacy.Preference, error) {
	rows, err := s.q.ListPreferencesForPrivacy(ctx, notificationsqlc.ListPreferencesForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]notificationsprivacy.Preference, len(rows))
	for i, row := range rows {
		out[i] = notificationsprivacy.Preference{
			Type: row.Type, Channel: row.Channel, Enabled: row.Enabled, UpdatedAt: row.UpdatedAt.Time,
		}
	}
	return out, nil
}

func (s *PrivacyStore) DeleteAll(ctx context.Context, tenantID, userID int64) error {
	if err := s.q.DeleteNotificationPreferencesForPrivacy(ctx, notificationsqlc.DeleteNotificationPreferencesForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	}); err != nil {
		return err
	}
	return s.q.DeleteNotificationsForPrivacy(ctx, notificationsqlc.DeleteNotificationsForPrivacyParams{
		TenantID: tenantID, UserID: userID,
	})
}

func timestampPtr(t time.Time, valid bool) *time.Time {
	if !valid {
		return nil
	}
	return &t
}
