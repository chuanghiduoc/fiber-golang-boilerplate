// Package catalog declares the apikeys module's permissions. It is a leaf
// package so the module root and its subpackages can reference the
// constants; the module root registers them through app.PermissionProvider.
// Values are API-key scopes and must never change once released.
package catalog

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

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

// Register adds the module's permissions and error codes to the process-wide
// catalogs. app.RegisterCatalogs does the same through the module provider
// hooks; module unit tests that mount routes without the app call it directly.
// Repeating it is a no-op.
func Register() error {
	if err := authz.Register(Owner, Permissions()...); err != nil {
		return err
	}
	return apperror.Register(Owner, ErrorCodes())
}

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
