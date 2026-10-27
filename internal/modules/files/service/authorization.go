package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// The helpers below all go through authz.Authorize, the single authorization
// path: a subject (JWT or API key) is checked against the RBAC permission and
// the policies, a context marked by authz.SystemContext (trusted background
// callers) is allowed, and anything else is denied (fail closed). The calling
// methods keep their own tenant/owner checks as defense in depth.

// authorizeFile applies ABAC to an existing file: the tenant policy plus the
// owner policy, so a cross-tenant file or another user's file is concealed as
// 404.
func authorizeFile(a authz.Authorizer, ctx context.Context, permission authz.Permission, file *File) error {
	return a.Authorize(
		ctx,
		permission,
		authz.Resource{TenantID: file.TenantID, OwnerID: file.UserID},
		authz.TenantPolicy{},
		authz.OwnerPolicy{},
	)
}

// authorizeUserResource applies the owner+tenant policies when the file does
// not exist yet (upload) or when the operation is scoped to the caller's own
// collection (list).
func authorizeUserResource(a authz.Authorizer, ctx context.Context, permission authz.Permission, tenantID, userID uuid.UUID) error {
	return a.Authorize(
		ctx,
		permission,
		authz.Resource{TenantID: tenantID, OwnerID: userID},
		authz.TenantPolicy{},
		authz.OwnerPolicy{},
	)
}

// authorizeTenant applies only the tenant policy. Admin services use it so an
// administrator cannot cross the tenant boundary even with every admin
// permission.
func authorizeTenant(a authz.Authorizer, ctx context.Context, permission authz.Permission, tenantID uuid.UUID) error {
	return a.Authorize(
		ctx,
		permission,
		authz.Resource{TenantID: tenantID},
		authz.TenantPolicy{},
	)
}
