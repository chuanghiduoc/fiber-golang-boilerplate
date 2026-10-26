package notifications

import (
	"context"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const (
	deleteTenantNotificationPreferencesSQL = `DELETE FROM notification_preferences WHERE tenant_id = $1`
	deleteTenantNotificationsSQL           = `DELETE FROM notifications WHERE tenant_id = $1`
)

// tenantPurger removes the notification feed and channel preferences of a
// deleted tenant, children first so a failure leaves a resumable state.
type tenantPurger struct {
	db database.DBTX
}

// RegisterTenantPurge contributes notifications' tenant-wide purger to the
// tenancy registry. The composition root calls it once after Build.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry, db database.DBTX) {
	registry.Register(tenancycontracts.Provider{Name: "notifications", Purger: &tenantPurger{db: db}})
}

func (p *tenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	if _, err := p.db.Exec(ctx, deleteTenantNotificationPreferencesSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant notification preferences: %w", err)
	}
	if _, err := p.db.Exec(ctx, deleteTenantNotificationsSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant notifications: %w", err)
	}
	return nil
}

var _ tenancycontracts.Purger = (*tenantPurger)(nil)
