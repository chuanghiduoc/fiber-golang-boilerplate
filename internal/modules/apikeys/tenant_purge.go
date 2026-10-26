package apikeys

import (
	"context"
	"fmt"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const deleteTenantAPIKeysSQL = `DELETE FROM api_keys WHERE tenant_id = $1`

// tenantPurger removes every API key of a deleted tenant. api_key_scopes is a
// pure child of api_keys (CASCADE), so it needs no separate statement.
type tenantPurger struct {
	db database.DBTX
}

// RegisterTenantPurge contributes apikeys' tenant-wide purger to the tenancy
// registry. The composition root calls it once after Build.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry, db database.DBTX) {
	registry.Register(tenancycontracts.Provider{Name: "apikeys", Purger: &tenantPurger{db: db}})
}

func (p *tenantPurger) PurgeTenant(ctx context.Context, tenantID int64) error {
	ctx = tenant.WithID(ctx, tenantID)
	if _, err := p.db.Exec(ctx, deleteTenantAPIKeysSQL, tenantID); err != nil {
		return fmt.Errorf("delete tenant api keys: %w", err)
	}
	return nil
}

var _ tenancycontracts.Purger = (*tenantPurger)(nil)
