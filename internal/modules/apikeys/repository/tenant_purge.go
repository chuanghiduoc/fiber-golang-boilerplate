package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	apikeyssqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantPurger implements tenancy.Purger for API keys. It is the only apikeys
// repository that removes rows by tenant instead of by the request tenant.
type TenantPurger struct {
	q *apikeyssqlc.Queries
}

var _ tenancycontracts.Purger = (*TenantPurger)(nil)

// NewTenantPurger binds the tenant-wide purger to db (the RLS-scoped handle).
func NewTenantPurger(db apikeyssqlc.DBTX) *TenantPurger {
	return &TenantPurger{q: apikeyssqlc.New(db)}
}

// PurgeTenant deletes one bounded page of keys at a time until none remain.
// Re-running after a partial failure is safe: deleted pages are gone and the
// rest is selected again.
func (p *TenantPurger) PurgeTenant(ctx context.Context, tenantID uuid.UUID) error {
	ctx = tenant.WithID(ctx, tenantID)
	for {
		deleted, err := p.q.DeleteTenantAPIKeysBatch(ctx, apikeyssqlc.DeleteTenantAPIKeysBatchParams{
			TenantID:  tenantID,
			BatchSize: tenancycontracts.PurgeBatchSize,
		})
		if err != nil {
			return fmt.Errorf("delete tenant api keys: %w", err)
		}
		if deleted == 0 {
			return nil
		}
	}
}
