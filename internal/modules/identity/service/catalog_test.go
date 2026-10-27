package service

import (
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Instance catalogs built from identity's providers, exactly like the
// composition root. Package tests never mutate process-wide state.
var (
	testCatalog = newTestCatalog()
	testErrors  = newTestErrors()
)

func newTestCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: identitycatalog.Owner, Grants: identitycatalog.Permissions()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func newTestErrors() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: identitycatalog.Owner, Specs: identitycatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

// resolvedStatus renders the error's code through the test catalog, the same
// way the HTTP layer does.
func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := testErrors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
