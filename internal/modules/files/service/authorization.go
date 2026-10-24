package service

import (
	"context"

	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// authorizeFile applies ABAC to an existing file. Subject-authenticated
// requests (JWT or API key) get the tenant policy plus the owner policy, so a
// cross-tenant file or another user's file is concealed as 404. Direct service
// callers without a subject (unit tests, trusted background jobs) keep the
// legacy tenant/owner checks in the calling method.
func authorizeFile(ctx context.Context, permission authz.Permission, file *filesqlc.File) error {
	if _, ok := authz.SubjectFromContext(ctx); !ok {
		return nil
	}
	return authz.Authorize(
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
func authorizeUserResource(ctx context.Context, permission authz.Permission, tenantID, userID int64) error {
	if _, ok := authz.SubjectFromContext(ctx); !ok {
		return nil
	}
	return authz.Authorize(
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
func authorizeTenant(ctx context.Context, permission authz.Permission, tenantID int64) error {
	if _, ok := authz.SubjectFromContext(ctx); !ok {
		return nil
	}
	return authz.Authorize(
		ctx,
		permission,
		authz.Resource{TenantID: tenantID},
		authz.TenantPolicy{},
	)
}
