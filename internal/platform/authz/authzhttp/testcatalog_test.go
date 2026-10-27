package authzhttp

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// Platform and stand-in permission catalogs for package tests. They are
// instance-scoped like the composition root's catalog.
const (
	testPermissionRead  authz.Permission = "authzhttptest:read"
	testPermissionWrite authz.Permission = "authzhttptest:write"
)

var testCatalog = newTestCatalog()

func newTestCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: "authzhttptest", Grants: []authz.Grant{
		{Permission: testPermissionRead, Roles: []string{authz.RoleMember}},
		{Permission: testPermissionWrite, Roles: []string{authz.RoleMember}},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}
