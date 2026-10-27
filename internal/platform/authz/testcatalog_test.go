package authz

// Platform and stand-in permission catalogs for package tests. They are
// instance-scoped like the composition root's catalog, never process-wide
// registries.
const (
	testPermissionRead  Permission = "authztest:read"
	testPermissionWrite Permission = "authztest:write"
)

var testCatalog = newTestCatalog()

// testAuthorizer binds package tests to the stand-in catalog. The platform
// catalog alone grants the shared admin permissions used in policy tests.
func testAuthorizer() Authorizer { return NewAuthorizer(testCatalog) }

func newTestCatalog() *Catalog {
	catalog := NewCatalog()
	if err := catalog.Add(Provider{Owner: "authztest", Grants: []Grant{
		{Permission: testPermissionRead, Roles: []string{RoleMember}},
		{Permission: testPermissionWrite, Roles: []string{RoleMember}},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}
