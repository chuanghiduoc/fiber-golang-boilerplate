// Package catalog declares the webhooks module's permissions. It is a leaf
// package so the module root and its subpackages can reference the
// constants; the module root registers them through app.PermissionProvider.
// Values are API-key scopes and must never change once released.
package catalog

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Permissions owned by webhooks. Both are admin-only.
const (
	PermissionWebhooksRead  authz.Permission = "webhooks:read"
	PermissionWebhooksWrite authz.Permission = "webhooks:write"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "webhooks"

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

// Permissions returns the webhooks grants for app.PermissionProvider.
func Permissions() []authz.Grant {
	return []authz.Grant{
		{Permission: PermissionWebhooksRead},
		{Permission: PermissionWebhooksWrite},
	}
}
