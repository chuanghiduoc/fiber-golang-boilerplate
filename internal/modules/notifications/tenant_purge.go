package notifications

import (
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
)

// RegisterTenantPurge contributes notifications' tenant-wide purger to the
// tenancy registry. The composition root calls it once after Build.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry) {
	if m.tenantPurger == nil {
		return
	}
	registry.Register(tenancycontracts.Provider{Name: "notifications", Purger: m.tenantPurger})
}
