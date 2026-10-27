// Package catalog declares the apikeys module's permissions. It is a leaf
// package so the module root and its subpackages can reference the
// constants; the module root registers them through app.PermissionProvider.
// Values are API-key scopes and must never change once released.
package catalog

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// Self-service permissions for the caller's own keys under /me/api-keys. Every
// member receives them: the routes never reach another member's key.
const (
	PermissionAPIKeysRead   authz.Permission = "api_keys:read"
	PermissionAPIKeysCreate authz.Permission = "api_keys:create"
	PermissionAPIKeysUpdate authz.Permission = "api_keys:update"
	PermissionAPIKeysRevoke authz.Permission = "api_keys:revoke"
	PermissionAPIKeysRotate authz.Permission = "api_keys:rotate"
)

// Permissions for managing another member's keys under
// /members/{memberId}/api-keys. Owner and admin receive every tenant
// permission by construction; members do not.
const (
	PermissionAdminAPIKeysRead   authz.Permission = "admin:api_keys:read"
	PermissionAdminAPIKeysCreate authz.Permission = "admin:api_keys:create"
	PermissionAdminAPIKeysUpdate authz.Permission = "admin:api_keys:update"
	PermissionAdminAPIKeysRevoke authz.Permission = "admin:api_keys:revoke"
	PermissionAdminAPIKeysRotate authz.Permission = "admin:api_keys:rotate"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "apikeys"

// Permissions returns the apikeys grants for app.PermissionProvider.
func Permissions() []authz.Grant {
	return []authz.Grant{
		{Permission: PermissionAPIKeysRead, Roles: []string{authz.RoleMember}},
		{Permission: PermissionAPIKeysCreate, Roles: []string{authz.RoleMember}},
		{Permission: PermissionAPIKeysUpdate, Roles: []string{authz.RoleMember}},
		{Permission: PermissionAPIKeysRevoke, Roles: []string{authz.RoleMember}},
		{Permission: PermissionAPIKeysRotate, Roles: []string{authz.RoleMember}},
		// admin:api_keys:create and admin:api_keys:rotate stay registered even
		// though no route uses them now: permission values are stable API-key
		// scopes, and a released scope must keep resolving in the catalog.
		{Permission: PermissionAdminAPIKeysRead},
		{Permission: PermissionAdminAPIKeysCreate},
		{Permission: PermissionAdminAPIKeysUpdate},
		{Permission: PermissionAdminAPIKeysRevoke},
		{Permission: PermissionAdminAPIKeysRotate},
	}
}
