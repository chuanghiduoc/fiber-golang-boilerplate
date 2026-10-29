//go:build integration

package membership_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fieldAuthActorSpec struct {
	roleKey    string
	email      string
	permission authz.Permission
}

func createFieldAuthActor(t *testing.T, e *pgfixtures.Env, ctx context.Context, spec fieldAuthActorSpec) testutil.Member {
	t.Helper()
	_, err := e.Roles.Create(ctx, core.NewRole{
		Key:         spec.roleKey,
		Name:        spec.roleKey,
		Permissions: []string{string(spec.permission)},
	})
	require.NoError(t, err)
	actor := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, spec.email, testutil.MemberOptions{
		Role: core.RoleMember,
	})
	_, err = e.Roles.ReplaceMemberRoles(ctx, actor.UserID, []string{spec.roleKey})
	require.NoError(t, err)
	return actor
}

func requirePermissionDenied(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))
	assert.Equal(t, string(apperror.CodePermissionDenied), pgfixtures.AppCode(err))
}

func requireMemberState(t *testing.T, e *pgfixtures.Env, ctx context.Context, userID uuid.UUID, status core.MembershipStatus, roles []string) {
	t.Helper()
	current, err := e.Stores.Memberships.Get(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, status, current.Membership.Status)
	assert.ElementsMatch(t, roles, current.Membership.Roles)
}

func TestUpdateMember_FieldPermissionsFailBeforeMutation(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	const targetRoleKey = "field_auth_target"

	_, err := e.Roles.Create(ctx, core.NewRole{Key: targetRoleKey, Name: "Field Auth Target"})
	require.NoError(t, err)

	roleOnly := createFieldAuthActor(t, e, ctx, fieldAuthActorSpec{
		roleKey: "field_auth_role_only", email: "field-role-only@example.com",
		permission: authz.PermissionAdminUsersRole,
	})
	banOnly := createFieldAuthActor(t, e, ctx, fieldAuthActorSpec{
		roleKey: "field_auth_ban_only", email: "field-ban-only@example.com",
		permission: authz.PermissionAdminUsersBan,
	})
	deleteOnly := createFieldAuthActor(t, e, ctx, fieldAuthActorSpec{
		roleKey: "field_auth_delete_only", email: "field-delete-only@example.com",
		permission: authz.PermissionAdminUsersDelete,
	})
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "field-owner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})

	roleOnlyCtx := memberContextWithPermissions(roleOnly.UserID, core.RoleMember, authz.PermissionAdminUsersRole)
	banOnlyCtx := memberContextWithPermissions(banOnly.UserID, core.RoleMember, authz.PermissionAdminUsersBan)
	deleteOnlyCtx := memberContextWithPermissions(deleteOnly.UserID, core.RoleMember, authz.PermissionAdminUsersDelete)
	ownerCtx := memberContext(owner.UserID, core.RoleOwner)

	suspended := string(core.MembershipStatusSuspended)
	removed := string(core.MembershipStatusRemoved)

	denied := []struct {
		name   string
		ctx    context.Context
		email  string
		update membership.MemberUpdate
	}{
		{
			name: "role only cannot patch status", ctx: roleOnlyCtx, email: "field-role-status@example.com",
			update: membership.MemberUpdate{Status: &suspended},
		},
		{
			name: "ban only cannot patch roles", ctx: banOnlyCtx, email: "field-ban-roles@example.com",
			update: membership.MemberUpdate{RoleKeys: []string{targetRoleKey}},
		},
		{
			name: "delete only cannot patch a ban", ctx: deleteOnlyCtx, email: "field-delete-ban@example.com",
			update: membership.MemberUpdate{Status: &suspended},
		},
		{
			name: "ban only cannot remove", ctx: banOnlyCtx, email: "field-ban-remove@example.com",
			update: membership.MemberUpdate{Status: &removed},
		},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			target := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, tc.email,
				testutil.MemberOptions{Role: core.RoleMember})
			_, err := e.Admin.UpdateMember(tc.ctx, target.UserID, tc.update)
			requirePermissionDenied(t, err)
			requireMemberState(t, e, ctx, target.UserID, core.MembershipStatusActive, []string{core.RoleMember})
		})
	}

	allowed := []struct {
		name       string
		ctx        context.Context
		email      string
		update     membership.MemberUpdate
		wantStatus core.MembershipStatus
		wantRoles  []string
	}{
		{
			name: "role only can patch roles", ctx: roleOnlyCtx, email: "field-role-allowed@example.com",
			update:     membership.MemberUpdate{RoleKeys: []string{targetRoleKey}},
			wantStatus: core.MembershipStatusActive, wantRoles: []string{targetRoleKey},
		},
		{
			name: "ban only can patch status", ctx: banOnlyCtx, email: "field-ban-allowed@example.com",
			update:     membership.MemberUpdate{Status: &suspended},
			wantStatus: core.MembershipStatusSuspended, wantRoles: []string{core.RoleMember},
		},
		{
			name: "delete only can remove", ctx: deleteOnlyCtx, email: "field-delete-allowed@example.com",
			update:     membership.MemberUpdate{Status: &removed},
			wantStatus: core.MembershipStatusRemoved,
		},
		{
			name: "owner has every field grant", ctx: ownerCtx, email: "field-owner-allowed@example.com",
			update: membership.MemberUpdate{
				RoleKeys: []string{targetRoleKey},
				Status:   &suspended,
			},
			wantStatus: core.MembershipStatusSuspended, wantRoles: []string{targetRoleKey},
		},
	}
	for _, tc := range allowed {
		t.Run(tc.name, func(t *testing.T) {
			target := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, tc.email,
				testutil.MemberOptions{Role: core.RoleMember})
			_, err := e.Admin.UpdateMember(tc.ctx, target.UserID, tc.update)
			require.NoError(t, err)
			requireMemberState(t, e, ctx, target.UserID, tc.wantStatus, tc.wantRoles)
		})
	}
}
