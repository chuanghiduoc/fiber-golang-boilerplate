// Package catalog declares the apikeys module's permissions. It is a leaf
// package so the module root and its subpackages can reference the
// constants; the module root registers them through app.PermissionProvider.
// Values are API-key scopes and must never change once released.
package catalog

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// Permissions owned by API-key administration. All are admin-only.
const (
	PermissionAdminAPIKeysRead   authz.Permission = "admin:api_keys:read"
	PermissionAdminAPIKeysCreate authz.Permission = "admin:api_keys:create"
	PermissionAdminAPIKeysUpdate authz.Permission = "admin:api_keys:update"
	PermissionAdminAPIKeysRevoke authz.Permission = "admin:api_keys:revoke"
	PermissionAdminAPIKeysRotate authz.Permission = "admin:api_keys:rotate"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "apikeys"

// Register adds the permissions to the authz catalog. app.RegisterCatalogs
// does the same at startup through app.PermissionProvider; module unit tests
// that mount routes without the app call it directly. Repeating it is a no-op.
func Register() error { return authz.Register(Owner, Permissions()...) }

// Permissions returns the apikeys grants for app.PermissionProvider.
func Permissions() []authz.Grant {
	return []authz.Grant{
		{Permission: PermissionAdminAPIKeysRead},
		{Permission: PermissionAdminAPIKeysCreate},
		{Permission: PermissionAdminAPIKeysUpdate},
		{Permission: PermissionAdminAPIKeysRevoke},
		{Permission: PermissionAdminAPIKeysRotate},
	}
}
