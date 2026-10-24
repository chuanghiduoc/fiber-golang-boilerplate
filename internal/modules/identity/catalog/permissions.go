package catalog

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// Permissions owned by identity. The admin:users:* permissions are shared
// with the admin module and stay in internal/platform/authz.
const (
	PermissionProfileRead    authz.Permission = "profile:read"
	PermissionProfileWrite   authz.Permission = "profile:write"
	PermissionSessionsRevoke authz.Permission = "sessions:revoke"
)

// Permissions returns identity's grants for app.PermissionProvider.
func Permissions() []authz.Grant {
	user := []string{authz.RoleUser}
	return []authz.Grant{
		{Permission: PermissionProfileRead, Roles: user},
		{Permission: PermissionProfileWrite, Roles: user},
		{Permission: PermissionSessionsRevoke, Roles: user},
	}
}
