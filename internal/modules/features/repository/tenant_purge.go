package repository

import (
	"context"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	featuressqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantPurger implements tenancy.Purger for feature-flag targets. The
// process-wide feature_flags table is deliberately untouched.
type TenantPurger struct {
	q *featuressqlc.Queries
}

var _ tenancycontracts.Purger = (*TenantPurger)(nil)

// NewTenantPurger binds the tenant-wide purger to db (the RLS-scoped handle).
func NewTenantPurger(db featuressqlc.DBTX) *TenantPurger {
	return &TenantPurger{q: featuressqlc.New(db)}
}

// PurgeTenant deletes one bounded page of targets at a time until none remain.
func (p *TenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	for {
		deleted, err := p.q.DeleteTenantFeatureFlagTargetsBatch(ctx, featuressqlc.DeleteTenantFeatureFlagTargetsBatchParams{
			TenantID:  tenantID,
			BatchSize: tenancycontracts.PurgeBatchSize,
		})
		if err != nil {
			return fmt.Errorf("delete tenant feature targets: %w", err)
		}
		if deleted == 0 {
			return nil
		}
	}
}
