package repository

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// NotificationRepository is the notifications module's persistence surface.
// Every method derives the request tenant from ctx and, for user-facing reads
// and writes, also filters by user ID so one user can never observe another
// user's feed or preferences in the same tenant.
type NotificationRepository interface {
	// WithTx returns a repository bound to tx so a notification row and its
	// email job can commit atomically. A nil tx returns the original
	// repository, which keeps in-memory test fakes simple.
	WithTx(tx pgx.Tx) NotificationRepository
	Create(ctx context.Context, params notificationsqlc.CreateNotificationParams) (*notificationsqlc.Notification, error)
	GetByID(ctx context.Context, id, userID int64) (*notificationsqlc.Notification, error)
	ListCursor(ctx context.Context, userID int64, cur pagination.DBCursor, unreadOnly bool) ([]notificationsqlc.Notification, error)
	CountUnread(ctx context.Context, userID int64) (int64, error)
	MarkRead(ctx context.Context, id, userID int64) (*notificationsqlc.Notification, error)
	MarkAllRead(ctx context.Context, userID int64) (int64, error)
	GetPreference(ctx context.Context, userID int64, notificationType, channel string) (*notificationsqlc.NotificationPreference, error)
	ListPreferences(ctx context.Context, userID int64) ([]notificationsqlc.NotificationPreference, error)
	UpsertPreference(ctx context.Context, params notificationsqlc.UpsertNotificationPreferenceParams) (*notificationsqlc.NotificationPreference, error)
}

type notificationRepository struct {
	q *notificationsqlc.Queries
}

func NewNotificationRepository(db notificationsqlc.DBTX) NotificationRepository {
	return &notificationRepository{q: notificationsqlc.New(db)}
}

func (r *notificationRepository) WithTx(tx pgx.Tx) NotificationRepository {
	if tx == nil {
		return r
	}
	return &notificationRepository{q: r.q.WithTx(tx)}
}

func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

func (r *notificationRepository) Create(ctx context.Context, params notificationsqlc.CreateNotificationParams) (*notificationsqlc.Notification, error) {
	// The context tenant is authoritative; a caller cannot write a notification
	// into another tenant by setting the generated field.
	params.TenantID = tenantID(ctx)
	row, err := r.q.CreateNotification(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &row, nil
}

func (r *notificationRepository) GetByID(ctx context.Context, id, userID int64) (*notificationsqlc.Notification, error) {
	row, err := r.q.GetNotificationByID(ctx, notificationsqlc.GetNotificationByIDParams{
		ID:       id,
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &row, nil
}

func (r *notificationRepository) ListCursor(ctx context.Context, userID int64, cur pagination.DBCursor, unreadOnly bool) ([]notificationsqlc.Notification, error) {
	return r.q.ListNotificationsCursor(ctx, notificationsqlc.ListNotificationsCursorParams{
		TenantID:        tenantID(ctx),
		UserID:          userID,
		UnreadOnly:      unreadOnly,
		HasCursor:       cur.HasCursor,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.ID,
		RowLimit:        cur.Limit,
	})
}

func (r *notificationRepository) CountUnread(ctx context.Context, userID int64) (int64, error) {
	return r.q.CountUnreadNotifications(ctx, notificationsqlc.CountUnreadNotificationsParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
}

func (r *notificationRepository) MarkRead(ctx context.Context, id, userID int64) (*notificationsqlc.Notification, error) {
	row, err := r.q.MarkNotificationRead(ctx, notificationsqlc.MarkNotificationReadParams{
		ID:       id,
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &row, nil
}

func (r *notificationRepository) MarkAllRead(ctx context.Context, userID int64) (int64, error) {
	return r.q.MarkAllNotificationsRead(ctx, notificationsqlc.MarkAllNotificationsReadParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
}

func (r *notificationRepository) GetPreference(ctx context.Context, userID int64, notificationType, channel string) (*notificationsqlc.NotificationPreference, error) {
	row, err := r.q.GetNotificationPreference(ctx, notificationsqlc.GetNotificationPreferenceParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
		Type:     notificationType,
		Channel:  channel,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &row, nil
}

func (r *notificationRepository) ListPreferences(ctx context.Context, userID int64) ([]notificationsqlc.NotificationPreference, error) {
	return r.q.ListNotificationPreferences(ctx, notificationsqlc.ListNotificationPreferencesParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
}

func (r *notificationRepository) UpsertPreference(ctx context.Context, params notificationsqlc.UpsertNotificationPreferenceParams) (*notificationsqlc.NotificationPreference, error) {
	params.TenantID = tenantID(ctx)
	row, err := r.q.UpsertNotificationPreference(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &row, nil
}

// wrapErr translates pgx "no rows" into the shared sentinel so services do
// not depend on database-driver details.
func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	return err
}
