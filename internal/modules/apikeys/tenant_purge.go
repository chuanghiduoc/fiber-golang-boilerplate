package apikeys

import (
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
)

// TenantPurgers contributes apikeys' tenant-wide purger to the composition
// root's tenancy registry.
func (m *Module) TenantPurgers() []tenancycontracts.Provider {
	if m.tenantPurger == nil {
		return nil
	}
	return []tenancycontracts.Provider{{Name: "apikeys", Purger: m.tenantPurger}}
}
