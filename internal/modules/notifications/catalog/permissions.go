// Package catalog declares the notifications module's permissions. It is a
// leaf package so the module root and its subpackages can reference the
// constants; the module root registers them through app.PermissionProvider.
// Values are API-key scopes and must never change once released.
package catalog

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// Permissions owned by notifications.
const (
	PermissionNotificationsRead  authz.Permission = "notifications:read"
	PermissionNotificationsWrite authz.Permission = "notifications:write"
	// PermissionNotificationsSend is admin-only: it allows creating a
	// notification for another user through the back-office endpoint.
	PermissionNotificationsSend authz.Permission = "notifications:send"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "notifications"

// Register adds the permissions to the authz catalog. app.RegisterCatalogs
// does the same at startup through app.PermissionProvider; module unit tests
// that mount routes without the app call it directly. Repeating it is a no-op.
func Register() error { return authz.Register(Owner, Permissions()...) }

// Permissions returns the notifications grants for app.PermissionProvider.
func Permissions() []authz.Grant {
	user := []string{authz.RoleUser}
	return []authz.Grant{
		{Permission: PermissionNotificationsRead, Roles: user},
		{Permission: PermissionNotificationsWrite, Roles: user},
		{Permission: PermissionNotificationsSend},
	}
}
