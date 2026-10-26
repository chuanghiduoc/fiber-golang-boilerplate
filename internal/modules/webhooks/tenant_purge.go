package webhooks

import (
	"context"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const (
	deleteTenantWebhookDeliveriesSQL = `DELETE FROM webhook_deliveries WHERE tenant_id = $1`
	deleteTenantWebhookEventsSQL     = `DELETE FROM webhook_events WHERE tenant_id = $1`
	deleteTenantWebhookEndpointsSQL  = `DELETE FROM webhook_endpoints WHERE tenant_id = $1`
	deleteTenantInboundEventsSQL     = `DELETE FROM webhook_inbound_events WHERE tenant_id = $1`
	deleteTenantInboundSourcesSQL    = `DELETE FROM webhook_inbound_sources WHERE tenant_id = $1`
)

// tenantPurger removes a deleted tenant's outbound endpoints, delivery
// history, and inbound sources/events. Deliveries go before events and events
// before endpoints because both snapshot FKs are RESTRICT; attempts,
// subscriptions, and secrets cascade from their parent.
type tenantPurger struct {
	db database.DBTX
}

// RegisterTenantPurge contributes webhooks' tenant-wide purger to the tenancy
// registry. The composition root calls it once after Build.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry, db database.DBTX) {
	registry.Register(tenancycontracts.Provider{Name: "webhooks", Purger: &tenantPurger{db: db}})
}

func (p *tenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	if _, err := p.db.Exec(ctx, deleteTenantWebhookDeliveriesSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant webhook deliveries: %w", err)
	}
	if _, err := p.db.Exec(ctx, deleteTenantWebhookEventsSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant webhook events: %w", err)
	}
	if _, err := p.db.Exec(ctx, deleteTenantWebhookEndpointsSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant webhook endpoints: %w", err)
	}
	if _, err := p.db.Exec(ctx, deleteTenantInboundEventsSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant webhook inbound events: %w", err)
	}
	if _, err := p.db.Exec(ctx, deleteTenantInboundSourcesSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant webhook inbound sources: %w", err)
	}
	return nil
}

var _ tenancycontracts.Purger = (*tenantPurger)(nil)
