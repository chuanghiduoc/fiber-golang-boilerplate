package service

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// The scope tests use real-looking files:* scopes. The files module owns and
// registers them at startup, and apikeys cannot import it, so this test binary
// registers identical stand-ins under the same owner.
const (
	testScopeFilesRead  authz.Permission = "files:read"
	testScopeFilesWrite authz.Permission = "files:write"
)

func init() {
	user := []string{authz.RoleUser}
	if err := authz.Register("files",
		authz.Grant{Permission: testScopeFilesRead, Roles: user},
		authz.Grant{Permission: testScopeFilesWrite, Roles: user},
	); err != nil {
		panic(err)
	}
}
