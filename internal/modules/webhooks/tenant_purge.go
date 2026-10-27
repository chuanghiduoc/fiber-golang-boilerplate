package webhooks

import (
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
)

// TenantPurgers contributes webhooks' tenant-wide purger to the composition
// root's tenancy registry.
func (m *Module) TenantPurgers() []tenancycontracts.Provider {
	if m.tenantPurger == nil {
		return nil
	}
	return []tenancycontracts.Provider{{Name: "webhooks", Purger: m.tenantPurger}}
}
