package service

import (
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Instance catalogs built from the files module's providers, exactly like the
// composition root. Package tests never mutate process-wide state.
var (
	testCatalog = newTestCatalog()
	testErrors  = newTestErrors()
)

func newTestCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: filescatalog.Owner, Grants: filescatalog.Permissions()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func newTestErrors() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: filescatalog.Owner, Specs: filescatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func testAuthorizer() authz.Authorizer { return authz.NewAuthorizer(testCatalog) }

// resolvedStatus renders the error's code through the test catalog, the same
// way the HTTP layer does.
func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := testErrors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
