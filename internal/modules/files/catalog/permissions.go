package catalog

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// Permissions owned by files. The admin:files:* and admin:stats:read
// permissions are shared with the admin module and stay in
// internal/platform/authz.
const (
	PermissionFilesRead    authz.Permission = "files:read"
	PermissionFilesWrite   authz.Permission = "files:write"
	PermissionFilesDelete  authz.Permission = "files:delete"
	PermissionFilesRestore authz.Permission = "files:restore"
)

// Permissions returns the files grants for app.PermissionProvider.
func Permissions() []authz.Grant {
	user := []string{authz.RoleUser}
	return []authz.Grant{
		{Permission: PermissionFilesRead, Roles: user},
		{Permission: PermissionFilesWrite, Roles: user},
		{Permission: PermissionFilesDelete, Roles: user},
		{Permission: PermissionFilesRestore, Roles: user},
	}
}
