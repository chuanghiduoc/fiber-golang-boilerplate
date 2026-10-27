//go:build integration

package membership_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

func memberContext(userID uuid.UUID, role string) context.Context {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	granted := pgfixtures.Catalog.PermissionsForRole(role)
	permissions := make([]authz.Permission, 0, len(granted))
	for permission := range granted {
		permissions = append(permissions, permission)
	}
	return authz.WithSubject(ctx, authz.NewSubjectWithPermissions(userID, tenant.DefaultID, role, permissions...))
}

func memberContextWithPermissions(userID uuid.UUID, role string, permissions ...authz.Permission) context.Context {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	return authz.WithSubject(ctx, authz.NewSubjectWithPermissions(userID, tenant.DefaultID, role, permissions...))
}

// A tenant-defined role that grants a member-management permission has the
// admin rank: it manages members but not admins or owners, and it still needs
// every permission of a role it grants (so it cannot mint a system admin).
func TestRoleHierarchy_DelegatedAdminManagesMembersNotPeers(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "delegated-owner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	admin := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "delegated-admin@example.com",
		testutil.MemberOptions{Role: core.RoleAdmin})
	delegated := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "delegated-manager@example.com",
		testutil.MemberOptions{})
	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "delegated-member@example.com",
		testutil.MemberOptions{Role: core.RoleMember})
	target := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "delegated-target@example.com",
		testutil.MemberOptions{Role: core.RoleMember})

	// The delegated role holds every member grant (so it may grant member) plus
	// the member-management permissions that give it the admin rank.
	delegatedGrants := []authz.Permission{authz.PermissionAdminUsersRole, authz.PermissionAdminUsersBan, authz.PermissionAdminUsersDelete}
	for permission := range pgfixtures.Catalog.PermissionsForRole(core.RoleMember) {
		delegatedGrants = append(delegatedGrants, permission)
	}
	memberPermissions := make([]string, 0, len(delegatedGrants))
	for _, permission := range delegatedGrants {
		memberPermissions = append(memberPermissions, string(permission))
	}
	_, err := e.Stores.Roles.Create(ctx, core.NewRole{
		Key: "delegated_admin", Name: "Delegated Admin", Permissions: memberPermissions,
	})
	require.NoError(t, err)
	require.NoError(t, e.Stores.Roles.ReplaceMemberRoles(ctx, delegated.UserID, []string{"delegated_admin"}))

	delegatedCtx := memberContextWithPermissions(delegated.UserID, core.RoleMember, delegatedGrants...)

	require.NoError(t, e.Admin.BanUser(delegatedCtx, member.UserID),
		"a delegated admin manages a member")
	_, err = e.Admin.UpdateRole(delegatedCtx, target.UserID, core.RoleMember)
	require.NoError(t, err, "it may grant a role whose permissions it holds")

	_, err = e.Admin.UpdateRole(delegatedCtx, admin.UserID, core.RoleMember)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"a delegated admin cannot manage a system admin")
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))

	_, err = e.Admin.UpdateRole(delegatedCtx, owner.UserID, core.RoleMember)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"a delegated admin cannot manage an owner")

	_, err = e.Admin.UpdateRole(delegatedCtx, target.UserID, core.RoleAdmin)
	assert.Equal(t, string(apperror.CodePermissionDenied), pgfixtures.AppCode(err),
		"granting a system admin needs every admin permission, not just rank")
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))

	// The escalation attempt changed nothing.
	got, err := e.Stores.Memberships.Get(tenant.WithID(ctx, tenant.DefaultID), target.UserID)
	require.NoError(t, err)
	assert.Equal(t, core.RoleMember, got.Membership.Role)
}

func TestRoleHierarchy_AdminCannotGrantOwnerOrManageOwner(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "hierarchy-owner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	otherOwner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "hierarchy-owner-2@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	admin := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "hierarchy-admin@example.com",
		testutil.MemberOptions{Role: core.RoleAdmin})
	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "hierarchy-member@example.com",
		testutil.MemberOptions{Role: core.RoleMember})

	ownerCtx := memberContext(owner.UserID, core.RoleOwner)
	adminCtx := memberContext(admin.UserID, core.RoleAdmin)

	_, err := e.Admin.UpdateRole(adminCtx, admin.UserID, core.RoleOwner)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"an admin cannot promote themselves to owner")
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))

	_, err = e.Admin.UpdateRole(adminCtx, member.UserID, core.RoleOwner)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"an admin cannot grant owner to another member")
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))

	_, err = e.Roles.ReplaceMemberRoles(adminCtx, member.UserID, []string{core.RoleOwner})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"an admin cannot replace a member's roles with owner")

	_, err = e.Admin.UpdateRole(adminCtx, member.UserID, core.RoleAdmin)
	require.NoError(t, err, "an admin may grant admin to a member")

	_, err = e.Admin.UpdateRole(adminCtx, owner.UserID, core.RoleMember)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"an admin cannot demote an owner")
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))

	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(e.Admin.BanUser(adminCtx, owner.UserID)),
		"an admin cannot ban an owner")

	_, err = e.Roles.ReplaceMemberRoles(adminCtx, owner.UserID, []string{core.RoleMember})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"an admin cannot replace an owner's roles")

	// Owners may manage other owners (co-owners) and grant owner.
	_, err = e.Admin.UpdateRole(ownerCtx, otherOwner.UserID, core.RoleAdmin)
	require.NoError(t, err, "an owner may demote a co-owner")
	_, err = e.Admin.UpdateRole(ownerCtx, admin.UserID, core.RoleOwner)
	require.NoError(t, err, "an owner may grant owner")

	got, err := e.Stores.Memberships.Get(tenant.WithID(ctx, tenant.DefaultID), admin.UserID)
	require.NoError(t, err)
	assert.Equal(t, core.RoleOwner, got.Membership.Role)
}

func TestRoleHierarchy_CustomRoleCannotDemoteOwner(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "custom-owner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	custom := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "custom-demoter@example.com",
		testutil.MemberOptions{})
	_, err := e.Stores.Roles.Create(ctx, core.NewRole{
		Key: "demoter", Name: "Demoter", Permissions: []string{string(authz.PermissionAdminUsersRole)},
	})
	require.NoError(t, err)
	require.NoError(t, e.Stores.Roles.ReplaceMemberRoles(ctx, custom.UserID, []string{"demoter"}))

	customCtx := authz.WithSubject(
		tenant.WithID(context.Background(), tenant.DefaultID),
		authz.NewSubjectWithPermissions(custom.UserID, tenant.DefaultID, core.RoleMember,
			authz.PermissionAdminUsersRole))
	_, err = e.Roles.ReplaceMemberRoles(customCtx, owner.UserID, []string{core.RoleMember})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"holding admin:users:role does not outrank an owner")
}

func TestInvitations_AdminCannotInviteOwner(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	admin := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "invite-admin@example.com",
		testutil.MemberOptions{Role: core.RoleAdmin})
	invitations := membership.NewInvitationService(e.Stores, e.Tx, "https://app.example.test")

	_, err := invitations.Invite(memberContext(admin.UserID, core.RoleAdmin), membership.InviteInput{
		InviterID: admin.UserID, Email: "owner-invite@example.com", Role: core.RoleOwner,
	})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err))
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))

	_, err = invitations.Invite(memberContext(admin.UserID, core.RoleAdmin), membership.InviteInput{
		InviterID: admin.UserID, Email: "member-invite@example.com", Role: core.RoleMember,
	})
	require.NoError(t, err, "an admin may still invite a member")
}

func TestInvitations_AcceptRechecksInviterAuthority(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviter := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "recheck-inviter@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	coOwner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "recheck-coowner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	otherTenant := testutil.CreateTenant(t, ctx, e.Pool, "recheck-other")
	invitee := testutil.CreateMember(t, ctx, e.Pool, otherTenant, "recheck-invitee@example.com",
		testutil.MemberOptions{})

	invitations := membership.NewInvitationService(e.Stores, e.Tx, "https://app.example.test")
	_, err := invitations.Invite(memberContext(inviter.UserID, core.RoleOwner), membership.InviteInput{
		InviterID: inviter.UserID, Email: invitee.Email, Role: core.RoleOwner,
	})
	require.NoError(t, err)
	token := e.Mail.TokenFor(t, invitee.Email)

	coOwnerCtx := memberContext(coOwner.UserID, core.RoleOwner)
	_, err = e.Admin.UpdateRole(coOwnerCtx, inviter.UserID, core.RoleAdmin)
	require.NoError(t, err, "a co-owner may demote the inviter")

	_, err = invitations.Accept(context.Background(), invitee.UserID, token)
	assert.Equal(t, string(identitycatalog.CodeInvitationInviterIneligible), pgfixtures.AppCode(err))
	assert.Equal(t, http.StatusConflict, pgfixtures.AppStatus(err))

	_, err = e.Stores.Memberships.Get(tenant.WithID(ctx, tenant.DefaultID), invitee.UserID)
	assert.Error(t, err, "the invitee must not join when the inviter lost authority")
}

func TestInvitations_SignUpRechecksInviterAuthority(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviter := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "signup-inviter@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	coOwner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "signup-coowner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})

	invitations := membership.NewInvitationService(e.Stores, e.Tx, "https://app.example.test")
	_, err := invitations.Invite(memberContext(inviter.UserID, core.RoleOwner), membership.InviteInput{
		InviterID: inviter.UserID, Email: "signup-invitee@example.com", Role: core.RoleOwner,
	})
	require.NoError(t, err)
	token := e.Mail.TokenFor(t, "signup-invitee@example.com")

	coOwnerCtx := memberContext(coOwner.UserID, core.RoleOwner)
	_, err = e.Admin.UpdateRole(coOwnerCtx, inviter.UserID, core.RoleAdmin)
	require.NoError(t, err, "a co-owner may demote the inviter")

	_, err = invitations.SignUp(ctx, membership.InvitationSignUp{
		Token: token, Name: "Invitee", Password: testPassphrase,
	})
	assert.Equal(t, string(identitycatalog.CodeInvitationInviterIneligible), pgfixtures.AppCode(err))
	assert.Equal(t, http.StatusConflict, pgfixtures.AppStatus(err))
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM users WHERE email = 'signup-invitee@example.com'`),
		"a refused signup must not create the account")
}

func TestInvitations_BanOrRemovalRevokesPendingInvitations(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	inviter := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "revoke-inviter@example.com",
		testutil.MemberOptions{Role: core.RoleAdmin})
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "revoke-owner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	invitations := membership.NewInvitationService(e.Stores, e.Tx, "https://app.example.test")
	invite := func(email string) {
		t.Helper()
		_, err := invitations.Invite(memberContext(inviter.UserID, core.RoleAdmin), membership.InviteInput{
			InviterID: inviter.UserID, Email: email, Role: core.RoleMember,
		})
		require.NoError(t, err)
	}
	invite("revoked-a@example.com")
	invite("revoked-b@example.com")
	assert.Equal(t, int64(2), scalar[int64](t, e.Pool,
		`SELECT count(*) FROM invitations WHERE tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND invited_by = $1 AND accepted_at IS NULL`, inviter.UserID))

	ownerCtx := memberContext(owner.UserID, core.RoleOwner)
	require.NoError(t, e.Admin.BanUser(ownerCtx, inviter.UserID))
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM invitations WHERE tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND invited_by = $1 AND accepted_at IS NULL`, inviter.UserID),
		"a ban revokes the inviter's pending invitations")

	// A removed inviter's invitations are revoked too.
	removed := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "revoke-removed@example.com",
		testutil.MemberOptions{Role: core.RoleAdmin})
	_, err := invitations.Invite(memberContext(removed.UserID, core.RoleAdmin), membership.InviteInput{
		InviterID: removed.UserID, Email: "revoked-c@example.com", Role: core.RoleMember,
	})
	require.NoError(t, err)
	require.NoError(t, e.Users.RemoveMember(ownerCtx, removed.UserID))
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM invitations WHERE tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND invited_by = $1 AND accepted_at IS NULL`, removed.UserID),
		"a removal revokes the inviter's pending invitations")
}
