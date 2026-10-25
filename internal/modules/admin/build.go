package admin

import (
	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	adminservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
)

// Build composes the back-office facade from the identity and files admin
// contracts. Admin owns no tables and no catalog entries: its routes check the
// shared admin:* permissions declared in internal/platform/authz because the
// identity and files services enforce the same permissions.
func Build(identity identitycontracts.IdentityAdmin, files filescontracts.FilesAdmin) *Module {
	return New(adminhandler.NewHandler(adminservice.NewService(identity, files)))
}
