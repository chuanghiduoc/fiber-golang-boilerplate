package catalog

import "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"

// Permissions owned by identity. The admin:users:* permissions are shared
// with the admin module and stay in internal/platform/authz.
const (
	PermissionProfileRead    authz.Permission = "profile:read"
	PermissionProfileWrite   authz.Permission = "profile:write"
	PermissionSessionsRevoke authz.Permission = "sessions:revoke"
	// PermissionMFARead reads the caller's MFA status.
	PermissionMFARead authz.Permission = "mfa:read"
	// PermissionMFAWrite enrolls, confirms, disables, and regenerates.
	PermissionMFAWrite authz.Permission = "mfa:write"
	// PermissionInvitationsRead lists the tenant's pending invitations.
	PermissionInvitationsRead authz.Permission = "invitations:read"
	// PermissionInvitationsWrite invites members and revokes invitations.
	PermissionInvitationsWrite authz.Permission = "invitations:write"
)

// Permissions returns identity's grants for app.PermissionProvider. Admins
// receive every permission; invitations are admin-only.
func Permissions() []authz.Grant {
	user := []string{authz.RoleUser}
	return []authz.Grant{
		{Permission: PermissionProfileRead, Roles: user},
		{Permission: PermissionProfileWrite, Roles: user},
		{Permission: PermissionSessionsRevoke, Roles: user},
		{Permission: PermissionMFARead, Roles: user},
		{Permission: PermissionMFAWrite, Roles: user},
		{Permission: PermissionInvitationsRead},
		{Permission: PermissionInvitationsWrite},
	}
}
