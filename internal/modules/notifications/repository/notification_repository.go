package repository

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	notificationservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// NotificationRepository implements service.NotificationRepository over sqlc.
// Every method derives the request tenant from ctx and, for user-facing reads
// and writes, also filters by user ID so one user can never observe another
// user's feed or preferences in the same tenant.
type NotificationRepository struct {
	q *notificationsqlc.Queries
}

var _ notificationservice.NotificationRepository = (*NotificationRepository)(nil)

// NewNotificationRepository binds the repository to db: a pool for standalone
// reads and writes, or a pgx.Tx inside Transactor.InTx.
func NewNotificationRepository(db notificationsqlc.DBTX) *NotificationRepository {
	return &NotificationRepository{q: notificationsqlc.New(db)}
}

func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

func (r *NotificationRepository) Create(ctx context.Context, in notificationservice.NewNotification) (*notificationservice.Notification, error) {
	// The context tenant is authoritative; the input has no tenant field.
	row, err := r.q.CreateNotification(ctx, notificationsqlc.CreateNotificationParams{
		TenantID: tenantID(ctx),
		UserID:   in.UserID,
		Type:     in.Type,
		Title:    in.Title,
		Body:     in.Body,
		Data:     in.Data,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toNotification(row), nil
}

func (r *NotificationRepository) GetByID(ctx context.Context, id, userID int64) (*notificationservice.Notification, error) {
	row, err := r.q.GetNotificationByID(ctx, notificationsqlc.GetNotificationByIDParams{
		ID:       id,
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toNotification(row), nil
}

// ListCursor picks a dedicated query per filter: a boolean "unread only"
// parameter would hide the read_at IS NULL predicate from generic plans and
// keep them off the partial unread index.
func (r *NotificationRepository) ListCursor(ctx context.Context, userID int64, cur pagination.DBCursor, unreadOnly bool) ([]notificationservice.Notification, error) {
	if unreadOnly {
		rows, err := r.q.ListUnreadNotificationsCursor(ctx, notificationsqlc.ListUnreadNotificationsCursorParams{
			TenantID:        tenantID(ctx),
			UserID:          userID,
			CursorCreatedAt: cur.CreatedAtParam(),
			CursorID:        cur.IDParam(),
			RowLimit:        cur.Limit,
		})
		if err != nil {
			return nil, err
		}
		return toNotifications(rows), nil
	}
	rows, err := r.q.ListNotificationsCursor(ctx, notificationsqlc.ListNotificationsCursorParams{
		TenantID:        tenantID(ctx),
		UserID:          userID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, err
	}
	return toNotifications(rows), nil
}

func (r *NotificationRepository) CountUnread(ctx context.Context, userID int64) (int64, error) {
	return r.q.CountUnreadNotifications(ctx, notificationsqlc.CountUnreadNotificationsParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
}

func (r *NotificationRepository) MarkRead(ctx context.Context, id, userID int64) (*notificationservice.Notification, error) {
	row, err := r.q.MarkNotificationRead(ctx, notificationsqlc.MarkNotificationReadParams{
		ID:       id,
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toNotification(row), nil
}

func (r *NotificationRepository) MarkAllRead(ctx context.Context, userID int64) (int64, error) {
	return r.q.MarkAllNotificationsRead(ctx, notificationsqlc.MarkAllNotificationsReadParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
}

func (r *NotificationRepository) GetPreference(ctx context.Context, userID int64, notificationType, channel string) (*notificationservice.Preference, error) {
	row, err := r.q.GetNotificationPreference(ctx, notificationsqlc.GetNotificationPreferenceParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
		Type:     notificationType,
		Channel:  channel,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toPreference(row), nil
}

func (r *NotificationRepository) ListPreferences(ctx context.Context, userID int64) ([]notificationservice.Preference, error) {
	rows, err := r.q.ListNotificationPreferences(ctx, notificationsqlc.ListNotificationPreferencesParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, err
	}
	return toPreferences(rows), nil
}

func (r *NotificationRepository) UpsertPreference(ctx context.Context, in notificationservice.NewPreference) (*notificationservice.Preference, error) {
	row, err := r.q.UpsertNotificationPreference(ctx, notificationsqlc.UpsertNotificationPreferenceParams{
		TenantID: tenantID(ctx),
		UserID:   in.UserID,
		Type:     in.Type,
		Channel:  in.Channel,
		Enabled:  in.Enabled,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toPreference(row), nil
}

func toNotification(row notificationsqlc.Notification) *notificationservice.Notification {
	data := map[string]any{}
	// Create only stores validated JSON objects, so a decode failure means the
	// row was written outside the service.
	if len(row.Data) > 0 {
		if err := json.Unmarshal(row.Data, &data); err != nil {
			slog.Error("notification data is not a JSON object", slog.Int64("notification_id", row.ID), slog.Any("error", err))
		}
	}
	n := &notificationservice.Notification{
		ID:        row.ID,
		TenantID:  row.TenantID,
		UserID:    row.UserID,
		Type:      row.Type,
		Title:     row.Title,
		Body:      row.Body,
		Data:      data,
		CreatedAt: row.CreatedAt.Time,
	}
	if row.ReadAt.Valid {
		readAt := row.ReadAt.Time
		n.ReadAt = &readAt
	}
	return n
}

func toNotifications(rows []notificationsqlc.Notification) []notificationservice.Notification {
	out := make([]notificationservice.Notification, len(rows))
	for i := range rows {
		out[i] = *toNotification(rows[i])
	}
	return out
}

func toPreference(row notificationsqlc.NotificationPreference) *notificationservice.Preference {
	return &notificationservice.Preference{
		ID:        row.ID,
		TenantID:  row.TenantID,
		UserID:    row.UserID,
		Type:      row.Type,
		Channel:   row.Channel,
		Enabled:   row.Enabled,
		CreatedAt: row.CreatedAt.Time,
		UpdatedAt: row.UpdatedAt.Time,
	}
}

func toPreferences(rows []notificationsqlc.NotificationPreference) []notificationservice.Preference {
	out := make([]notificationservice.Preference, len(rows))
	for i := range rows {
		out[i] = *toPreference(rows[i])
	}
	return out
}

// wrapErr translates pgx "no rows" into the shared sentinel so services do
// not depend on database-driver details.
func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	return err
}
