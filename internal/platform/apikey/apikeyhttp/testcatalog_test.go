package apikeyhttp

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// testScopeRead is a user-granted permission in the package's instance
// catalog; module-owned scopes are contributed by their modules at runtime.
const testScopeRead authz.Permission = "apikeyhttptest:read"

var testCatalog = newTestCatalog()

func newTestCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: "apikeyhttptest", Grants: []authz.Grant{
		{Permission: testScopeRead, Roles: []string{authz.RoleMember}},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func testAuthorizer() authz.Authorizer { return authz.NewAuthorizer(testCatalog) }
