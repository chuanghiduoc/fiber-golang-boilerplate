package repository

import (
	"context"

	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
)

// RetentionRepository implements service.RetentionRepository (the
// assertion lives in the module's build.go: the legacy notification service
// imports this package, so importing the service here would be a cycle). It
// is the only notifications repository without a tenant predicate (see
// queries/retention.sql).
type RetentionRepository struct {
	q *notificationsqlc.Queries
}

func NewRetentionRepository(db notificationsqlc.DBTX) *RetentionRepository {
	return &RetentionRepository{q: notificationsqlc.New(db)}
}

func (r *RetentionRepository) PurgeRead(ctx context.Context, retentionDays, limit int32) (int64, error) {
	return r.q.PurgeReadNotifications(ctx, notificationsqlc.PurgeReadNotificationsParams{
		RetentionDays: retentionDays,
		BatchSize:     limit,
	})
}
