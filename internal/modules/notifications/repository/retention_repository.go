package repository

import (
	"context"

	notificationservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
)

// RetentionRepository implements service.RetentionRepository. It is the only
// notifications repository without a tenant predicate (see
// queries/retention.sql).
type RetentionRepository struct {
	q *notificationsqlc.Queries
}

var _ notificationservice.RetentionRepository = (*RetentionRepository)(nil)

func NewRetentionRepository(db notificationsqlc.DBTX) *RetentionRepository {
	return &RetentionRepository{q: notificationsqlc.New(db)}
}

func (r *RetentionRepository) PurgeRead(ctx context.Context, retentionDays, limit int32) (int64, error) {
	return r.q.PurgeReadNotifications(ctx, notificationsqlc.PurgeReadNotificationsParams{
		RetentionDays: retentionDays,
		BatchSize:     limit,
	})
}
