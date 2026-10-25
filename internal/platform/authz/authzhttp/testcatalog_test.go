package authzhttp

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// Module-owned permissions are registered at startup, so platform tests
// register their own user-granted permissions instead of borrowing a module's.
const (
	testPermissionRead  authz.Permission = "authzhttptest:read"
	testPermissionWrite authz.Permission = "authzhttptest:write"
)

func init() {
	if err := authz.Register("authzhttptest",
		authz.Grant{Permission: testPermissionRead, Roles: []string{authz.RoleUser}},
		authz.Grant{Permission: testPermissionWrite, Roles: []string{authz.RoleUser}},
	); err != nil {
		panic(err)
	}
}
