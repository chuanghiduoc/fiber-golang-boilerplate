package authz

// Module-owned permissions are registered at startup, so platform tests
// register their own user-granted permissions instead of borrowing a module's.
const (
	testPermissionRead  Permission = "authztest:read"
	testPermissionWrite Permission = "authztest:write"
)

func init() {
	if err := Register("authztest",
		Grant{Permission: testPermissionRead, Roles: []string{RoleMember}},
		Grant{Permission: testPermissionWrite, Roles: []string{RoleMember}},
	); err != nil {
		panic(err)
	}
}
