package features

import (
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
)

// TenantPurgers contributes features' tenant-wide purger to the composition
// root's tenancy registry.
func (m *Module) TenantPurgers() []tenancycontracts.Provider {
	if m.tenantPurger == nil {
		return nil
	}
	return []tenancycontracts.Provider{{Name: "features", Purger: m.tenantPurger}}
}
