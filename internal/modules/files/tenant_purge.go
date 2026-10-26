package files

import (
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
)

// RegisterTenantPurge contributes files' tenant-wide purger to the tenancy
// registry. The composition root calls it once after Build.
func (m *Module) RegisterTenantPurge(registry *tenancycontracts.Registry) {
	if m.tenantPurger == nil {
		return
	}
	registry.Register(tenancycontracts.Provider{Name: "files", Purger: m.tenantPurger})
}
