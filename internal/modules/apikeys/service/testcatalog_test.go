package service

import (
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// The scope tests use real-looking files:* scopes. The files module owns them
// at runtime; apikeys cannot import it, so this test catalog contributes
// identical stand-ins under the same owner plus the apikeys providers.
const (
	testScopeFilesRead  authz.Permission = "files:read"
	testScopeFilesWrite authz.Permission = "files:write"
)

var testAuthorizer = authz.NewAuthorizer(newTestCatalog())

func newTestCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: apikeyscatalog.Owner, Grants: apikeyscatalog.Permissions()}); err != nil {
		panic(err)
	}
	if err := catalog.Add(authz.Provider{Owner: "files", Grants: []authz.Grant{
		{Permission: testScopeFilesRead, Roles: []string{authz.RoleMember}},
		{Permission: testScopeFilesWrite, Roles: []string{authz.RoleMember}},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

var testErrors = newTestErrors()

func newTestErrors() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: apikeyscatalog.Owner, Specs: apikeyscatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := testErrors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
