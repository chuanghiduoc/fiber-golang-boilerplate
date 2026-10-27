package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	notificationsqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantPurger implements tenancy.Purger for the notification feed and its
// channel preferences. Preferences go first so a failure leaves a resumable
// state.
type TenantPurger struct {
	q *notificationsqlc.Queries
}

var _ tenancycontracts.Purger = (*TenantPurger)(nil)

// NewTenantPurger binds the tenant-wide purger to db (the RLS-scoped handle).
func NewTenantPurger(db notificationsqlc.DBTX) *TenantPurger {
	return &TenantPurger{q: notificationsqlc.New(db)}
}

// PurgeTenant drains both tables in bounded pages.
func (p *TenantPurger) PurgeTenant(ctx context.Context, tenantID uuid.UUID) error {
	ctx = tenant.WithID(ctx, tenantID)
	if err := purgeInBatches(ctx, func() (int64, error) {
		return p.q.DeleteTenantNotificationPreferencesBatch(ctx, notificationsqlc.DeleteTenantNotificationPreferencesBatchParams{
			TenantID:  tenantID,
			BatchSize: tenancycontracts.PurgeBatchSize,
		})
	}); err != nil {
		return fmt.Errorf("delete tenant notification preferences: %w", err)
	}
	if err := purgeInBatches(ctx, func() (int64, error) {
		return p.q.DeleteTenantNotificationsBatch(ctx, notificationsqlc.DeleteTenantNotificationsBatchParams{
			TenantID:  tenantID,
			BatchSize: tenancycontracts.PurgeBatchSize,
		})
	}); err != nil {
		return fmt.Errorf("delete tenant notifications: %w", err)
	}
	return nil
}

// purgeInBatches repeats one bounded delete until it reports no rows.
func purgeInBatches(ctx context.Context, batch func() (int64, error)) error {
	for {
		deleted, err := batch()
		if err != nil {
			return err
		}
		if deleted == 0 {
			return nil
		}
	}
}
