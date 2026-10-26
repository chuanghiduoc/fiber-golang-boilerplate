//go:build integration

package service_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

func memberContext(userID int64, role string) context.Context {
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	return authz.WithSubject(ctx, authz.NewSubject(userID, tenant.DefaultID, role))
}

func TestRoleHierarchy_AdminCannotGrantOwnerOrManageOwner(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	owner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "hierarchy-owner@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	otherOwner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "hierarchy-owner-2@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	admin := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "hierarchy-admin@example.com",
		testutil.MemberOptions{Role: identityservice.RoleAdmin})
	member := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "hierarchy-member@example.com",
		testutil.MemberOptions{Role: identityservice.RoleMember})

	ownerCtx := memberContext(owner.UserID, identityservice.RoleOwner)
	adminCtx := memberContext(admin.UserID, identityservice.RoleAdmin)

	_, err := e.admin.UpdateRole(adminCtx, admin.UserID, identityservice.RoleOwner)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(err),
		"an admin cannot promote themselves to owner")
	assert.Equal(t, http.StatusForbidden, appStatus(err))

	_, err = e.admin.UpdateRole(adminCtx, member.UserID, identityservice.RoleOwner)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(err),
		"an admin cannot grant owner to another member")
	assert.Equal(t, http.StatusForbidden, appStatus(err))

	_, err = e.roles.ReplaceMemberRoles(adminCtx, member.UserID, []string{identityservice.RoleOwner})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(err),
		"an admin cannot replace a member's roles with owner")

	_, err = e.admin.UpdateRole(adminCtx, member.UserID, identityservice.RoleAdmin)
	require.NoError(t, err, "an admin may grant admin to a member")

	_, err = e.admin.UpdateRole(adminCtx, owner.UserID, identityservice.RoleMember)
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(err),
		"an admin cannot demote an owner")
	assert.Equal(t, http.StatusForbidden, appStatus(err))

	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(e.admin.BanUser(adminCtx, owner.UserID)),
		"an admin cannot ban an owner")

	_, err = e.roles.ReplaceMemberRoles(adminCtx, owner.UserID, []string{identityservice.RoleMember})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(err),
		"an admin cannot replace an owner's roles")

	// Owners may manage other owners (co-owners) and grant owner.
	_, err = e.admin.UpdateRole(ownerCtx, otherOwner.UserID, identityservice.RoleAdmin)
	require.NoError(t, err, "an owner may demote a co-owner")
	_, err = e.admin.UpdateRole(ownerCtx, admin.UserID, identityservice.RoleOwner)
	require.NoError(t, err, "an owner may grant owner")

	got, err := e.stores.Memberships.Get(tenant.WithID(ctx, tenant.DefaultID), admin.UserID)
	require.NoError(t, err)
	assert.Equal(t, identityservice.RoleOwner, got.Membership.Role)
}

func TestRoleHierarchy_CustomRoleCannotDemoteOwner(t *testing.T) {
	e := newEnv(t, nil)
	ctx := authz.SystemContext(context.Background())
	owner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "custom-owner@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	custom := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "custom-demoter@example.com",
		testutil.MemberOptions{})
	_, err := e.stores.Roles.Create(ctx, identityservice.NewRole{
		Key: "demoter", Name: "Demoter", Permissions: []string{string(authz.PermissionAdminUsersRole)},
	})
	require.NoError(t, err)
	require.NoError(t, e.stores.Roles.ReplaceMemberRoles(ctx, custom.UserID, []string{"demoter"}))

	customCtx := authz.WithSubject(
		tenant.WithID(context.Background(), tenant.DefaultID),
		authz.NewSubjectWithPermissions(custom.UserID, tenant.DefaultID, identityservice.RoleMember,
			authz.PermissionAdminUsersRole))
	_, err = e.roles.ReplaceMemberRoles(customCtx, owner.UserID, []string{identityservice.RoleMember})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(err),
		"holding admin:users:role does not outrank an owner")
}

func TestInvitations_AdminCannotInviteOwner(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	admin := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "invite-admin@example.com",
		testutil.MemberOptions{Role: identityservice.RoleAdmin})
	invitations := identityservice.NewInvitationService(e.stores, e.tx, e.mail, "https://app.example.test")

	_, err := invitations.Invite(memberContext(admin.UserID, identityservice.RoleAdmin), identityservice.InviteInput{
		InviterID: admin.UserID, Email: "owner-invite@example.com", Role: identityservice.RoleOwner,
	})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), appCode(err))
	assert.Equal(t, http.StatusForbidden, appStatus(err))

	_, err = invitations.Invite(memberContext(admin.UserID, identityservice.RoleAdmin), identityservice.InviteInput{
		InviterID: admin.UserID, Email: "member-invite@example.com", Role: identityservice.RoleMember,
	})
	require.NoError(t, err, "an admin may still invite a member")
}

func TestInvitations_AcceptRechecksInviterAuthority(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	inviter := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "recheck-inviter@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	coOwner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "recheck-coowner@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	otherTenant := testutil.CreateTenant(t, ctx, e.pool, "recheck-other")
	invitee := testutil.CreateMember(t, ctx, e.pool, otherTenant, "recheck-invitee@example.com",
		testutil.MemberOptions{})

	invitations := identityservice.NewInvitationService(e.stores, e.tx, e.mail, "https://app.example.test")
	_, err := invitations.Invite(memberContext(inviter.UserID, identityservice.RoleOwner), identityservice.InviteInput{
		InviterID: inviter.UserID, Email: invitee.Email, Role: identityservice.RoleOwner,
	})
	require.NoError(t, err)
	token := e.mail.tokenFor(t, invitee.Email)

	coOwnerCtx := memberContext(coOwner.UserID, identityservice.RoleOwner)
	_, err = e.admin.UpdateRole(coOwnerCtx, inviter.UserID, identityservice.RoleAdmin)
	require.NoError(t, err, "a co-owner may demote the inviter")

	_, err = invitations.Accept(context.Background(), invitee.UserID, token)
	assert.Equal(t, string(identitycatalog.CodeInvitationInviterIneligible), appCode(err))
	assert.Equal(t, http.StatusConflict, appStatus(err))

	_, err = e.stores.Memberships.Get(tenant.WithID(ctx, tenant.DefaultID), invitee.UserID)
	assert.Error(t, err, "the invitee must not join when the inviter lost authority")
}

func TestInvitations_SignUpRechecksInviterAuthority(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	inviter := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "signup-inviter@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	coOwner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "signup-coowner@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})

	invitations := identityservice.NewInvitationService(e.stores, e.tx, e.mail, "https://app.example.test")
	_, err := invitations.Invite(memberContext(inviter.UserID, identityservice.RoleOwner), identityservice.InviteInput{
		InviterID: inviter.UserID, Email: "signup-invitee@example.com", Role: identityservice.RoleOwner,
	})
	require.NoError(t, err)
	token := e.mail.tokenFor(t, "signup-invitee@example.com")

	coOwnerCtx := memberContext(coOwner.UserID, identityservice.RoleOwner)
	_, err = e.admin.UpdateRole(coOwnerCtx, inviter.UserID, identityservice.RoleAdmin)
	require.NoError(t, err, "a co-owner may demote the inviter")

	_, err = invitations.SignUp(context.Background(), identityservice.InvitationSignUp{
		Token: token, Name: "Invitee", Password: "Password1!",
	})
	assert.Equal(t, string(identitycatalog.CodeInvitationInviterIneligible), appCode(err))
	assert.Equal(t, http.StatusConflict, appStatus(err))
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM users WHERE email = 'signup-invitee@example.com'`),
		"a refused signup must not create the account")
}

func TestInvitations_BanOrRemovalRevokesPendingInvitations(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	inviter := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "revoke-inviter@example.com",
		testutil.MemberOptions{Role: identityservice.RoleAdmin})
	owner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "revoke-owner@example.com",
		testutil.MemberOptions{Role: identityservice.RoleOwner})
	invitations := identityservice.NewInvitationService(e.stores, e.tx, e.mail, "https://app.example.test")
	invite := func(email string) {
		t.Helper()
		_, err := invitations.Invite(memberContext(inviter.UserID, identityservice.RoleAdmin), identityservice.InviteInput{
			InviterID: inviter.UserID, Email: email, Role: identityservice.RoleMember,
		})
		require.NoError(t, err)
	}
	invite("revoked-a@example.com")
	invite("revoked-b@example.com")
	assert.Equal(t, int64(2), scalar[int64](t, e.pool,
		`SELECT count(*) FROM invitations WHERE tenant_id = 1 AND invited_by = $1 AND accepted_at IS NULL`, inviter.UserID))

	ownerCtx := memberContext(owner.UserID, identityservice.RoleOwner)
	require.NoError(t, e.admin.BanUser(ownerCtx, inviter.UserID))
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM invitations WHERE tenant_id = 1 AND invited_by = $1 AND accepted_at IS NULL`, inviter.UserID),
		"a ban revokes the inviter's pending invitations")

	// A removed inviter's invitations are revoked too.
	removed := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "revoke-removed@example.com",
		testutil.MemberOptions{Role: identityservice.RoleAdmin})
	_, err := invitations.Invite(memberContext(removed.UserID, identityservice.RoleAdmin), identityservice.InviteInput{
		InviterID: removed.UserID, Email: "revoked-c@example.com", Role: identityservice.RoleMember,
	})
	require.NoError(t, err)
	require.NoError(t, e.users.RemoveMember(ownerCtx, removed.UserID))
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM invitations WHERE tenant_id = 1 AND invited_by = $1 AND accepted_at IS NULL`, removed.UserID),
		"a removal revokes the inviter's pending invitations")
}
