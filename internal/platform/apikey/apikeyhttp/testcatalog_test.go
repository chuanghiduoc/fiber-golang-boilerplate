package apikeyhttp

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// testScopeRead is a user-granted permission registered for these tests;
// module-owned scopes such as files:read are registered by their modules.
const testScopeRead authz.Permission = "apikeyhttptest:read"

func init() {
	if err := authz.Register("apikeyhttptest", authz.Grant{
		Permission: testScopeRead,
		Roles:      []string{authz.RoleUser},
	}); err != nil {
		panic(err)
	}
}
