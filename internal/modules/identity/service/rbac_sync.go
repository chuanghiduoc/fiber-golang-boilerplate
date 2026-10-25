package service

import (
	"context"
	"fmt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// SyncPermissions writes the code-owned permission catalog and the system
// roles' permission sets into the database. It is idempotent and safe to run
// from several replicas at startup.
func SyncPermissions(ctx context.Context, tx Transactor, roles RoleRepository) error {
	permissions := make([]CatalogPermission, 0, len(authz.AllPermissions()))
	for _, permission := range authz.AllPermissions() {
		owner, ok := authz.OwnerOf(permission)
		if !ok {
			return fmt.Errorf("identity: permission %q has no owner", permission)
		}
		permissions = append(permissions, CatalogPermission{
			Key:      string(permission),
			Owner:    owner,
			Platform: authz.IsPlatformPermission(permission),
		})
	}

	tenantPermissions := permissionKeys(authz.AllTenantPermissions())
	memberPermissions := make([]string, 0, len(tenantPermissions))
	for _, permission := range authz.AllTenantPermissions() {
		if authz.Has(authz.RoleMember, permission) {
			memberPermissions = append(memberPermissions, string(permission))
		}
	}
	platformPermissions := permissionKeys(authz.AllPlatformPermissions())

	err := tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		return tx.Roles.SyncCatalog(ctx, permissions, []CatalogRole{
			{Key: authz.RoleOwner, Name: "Owner", Scope: "tenant", Permissions: tenantPermissions},
			{Key: authz.RoleAdmin, Name: "Admin", Scope: "tenant", Permissions: tenantPermissions},
			{Key: authz.RoleMember, Name: "Member", Scope: "tenant", Permissions: memberPermissions},
			{Key: authz.RolePlatformAdmin, Name: "Platform Admin", Scope: "platform", Permissions: platformPermissions},
		})
	})
	if err != nil {
		return fmt.Errorf("sync permission catalog: %w", err)
	}
	return nil
}

func permissionKeys(permissions []authz.Permission) []string {
	keys := make([]string, len(permissions))
	for i, permission := range permissions {
		keys[i] = string(permission)
	}
	return keys
}
