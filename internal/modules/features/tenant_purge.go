package features

import (
	"context"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const deleteTenantFeatureTargetsSQL = `DELETE FROM feature_flag_targets WHERE tenant_id = $1`

// tenantPurger removes every feature-flag target of a deleted tenant. The
// process-wide feature_flags table is deliberately untouched.
type tenantPurger struct {
	db database.DBTX
}

// RegisterTenantPurge contributes features' tenant-wide purger to the tenancy
// registry. The composition root calls it once after Build.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry, db database.DBTX) {
	registry.Register(tenancycontracts.Provider{Name: "features", Purger: &tenantPurger{db: db}})
}

func (p *tenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	if _, err := p.db.Exec(ctx, deleteTenantFeatureTargetsSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant feature targets: %w", err)
	}
	return nil
}

var _ tenancycontracts.Purger = (*tenantPurger)(nil)
