package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	webhookssqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantPurger implements tenancy.Purger for a deleted tenant's webhook
// configuration and history. Deliveries go before events and events before
// endpoints because both snapshot foreign keys are RESTRICT; attempts,
// subscriptions, and secrets cascade from their parent.
type TenantPurger struct {
	q *webhookssqlc.Queries
}

var _ tenancycontracts.Purger = (*TenantPurger)(nil)

// NewTenantPurger binds the tenant-wide purger to db (the RLS-scoped handle).
func NewTenantPurger(db webhookssqlc.DBTX) *TenantPurger {
	return &TenantPurger{q: webhookssqlc.New(db)}
}

// PurgeTenant drains every owned table in bounded pages, in foreign-key order.
func (p *TenantPurger) PurgeTenant(ctx context.Context, tenantID uuid.UUID) error {
	ctx = tenant.WithID(ctx, tenantID)
	steps := []struct {
		name  string
		batch func() (int64, error)
	}{
		{"webhook deliveries", func() (int64, error) {
			return p.q.DeleteTenantWebhookDeliveriesBatch(ctx, webhookssqlc.DeleteTenantWebhookDeliveriesBatchParams{
				TenantID: tenantID, BatchSize: tenancycontracts.PurgeBatchSize,
			})
		}},
		{"webhook events", func() (int64, error) {
			return p.q.DeleteTenantWebhookEventsBatch(ctx, webhookssqlc.DeleteTenantWebhookEventsBatchParams{
				TenantID: tenantID, BatchSize: tenancycontracts.PurgeBatchSize,
			})
		}},
		{"webhook endpoints", func() (int64, error) {
			return p.q.DeleteTenantWebhookEndpointsBatch(ctx, webhookssqlc.DeleteTenantWebhookEndpointsBatchParams{
				TenantID: tenantID, BatchSize: tenancycontracts.PurgeBatchSize,
			})
		}},
		{"webhook inbound events", func() (int64, error) {
			return p.q.DeleteTenantWebhookInboundEventsBatch(ctx, webhookssqlc.DeleteTenantWebhookInboundEventsBatchParams{
				TenantID: tenantID, BatchSize: tenancycontracts.PurgeBatchSize,
			})
		}},
		{"webhook inbound sources", func() (int64, error) {
			return p.q.DeleteTenantWebhookInboundSourcesBatch(ctx, webhookssqlc.DeleteTenantWebhookInboundSourcesBatchParams{
				TenantID: tenantID, BatchSize: tenancycontracts.PurgeBatchSize,
			})
		}},
	}
	for _, step := range steps {
		if err := drainInBatches(ctx, step.batch); err != nil {
			return fmt.Errorf("delete tenant %s: %w", step.name, err)
		}
	}
	return nil
}

// drainInBatches repeats one bounded delete until it reports no rows.
func drainInBatches(ctx context.Context, batch func() (int64, error)) error {
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
