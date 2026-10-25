package apikey

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// testScopeRead is a user-granted permission registered for these tests;
// module-owned scopes such as files:read are registered by their modules.
const testScopeRead authz.Permission = "apikeytest:read"

func init() {
	if err := authz.Register("apikeytest", authz.Grant{
		Permission: testScopeRead,
		Roles:      []string{authz.RoleMember},
	}); err != nil {
		panic(err)
	}
}
