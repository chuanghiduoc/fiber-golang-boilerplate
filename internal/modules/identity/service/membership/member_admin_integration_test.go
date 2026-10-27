//go:build integration

package membership_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

func TestUpdateMember_AppliesRolesAndStatusAndChecksETag(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "member-owner@example.com", testutil.MemberOptions{Role: core.RoleOwner})
	target := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "member-target@example.com", testutil.MemberOptions{Role: core.RoleMember})

	current, err := e.Admin.GetMember(ctx, target.UserID)
	require.NoError(t, err)
	expected := current.UpdatedAt()

	status := string(core.MembershipStatusSuspended)
	updated, err := e.Admin.UpdateMember(ctx, target.UserID, membership.MemberUpdate{
		ExpectedUpdatedAt: expected,
		RoleKeys:          []string{core.RoleAdmin},
		Status:            &status,
	})
	require.NoError(t, err)
	assert.Equal(t, string(core.MembershipStatusSuspended), string(updated.Membership.Status))
	assert.NotNil(t, updated.Membership.BannedAt)
	assert.Equal(t, core.RoleAdmin, updated.Membership.Role)

	// The validator read before the mutation is now stale.
	_, err = e.Admin.UpdateMember(ctx, target.UserID, membership.MemberUpdate{
		ExpectedUpdatedAt: expected,
		Status:            &status,
	})
	assert.Equal(t, 412, pgfixtures.AppStatus(err))

	active := string(core.MembershipStatusActive)
	restored, err := e.Admin.UpdateMember(ctx, target.UserID, membership.MemberUpdate{Status: &active})
	require.NoError(t, err)
	assert.Equal(t, string(core.MembershipStatusActive), string(restored.Membership.Status))
	assert.Nil(t, restored.Membership.BannedAt)

	removed := string(core.MembershipStatusRemoved)
	_, err = e.Admin.UpdateMember(ctx, target.UserID, membership.MemberUpdate{Status: &removed})
	require.NoError(t, err)
	gone, err := e.Admin.GetMember(ctx, target.UserID)
	require.NoError(t, err)
	assert.Equal(t, string(core.MembershipStatusRemoved), string(gone.Membership.Status))
}

func TestListMembers_FiltersByStatusAndRole(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "list-owner@example.com", testutil.MemberOptions{Role: core.RoleOwner})
	testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "list-admin@example.com", testutil.MemberOptions{Role: core.RoleAdmin})
	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "list-member@example.com", testutil.MemberOptions{Role: core.RoleMember})
	require.NoError(t, e.Admin.BanUser(ctx, member.UserID))

	page, hasMore, err := e.Admin.ListMembers(ctx, 100, "", string(core.MembershipStatusSuspended), core.RoleMember)
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, page, 1)
	assert.Equal(t, member.UserID, page[0].User.ID)

	admins, _, err := e.Admin.ListMembers(ctx, 100, "", string(core.MembershipStatusActive), core.RoleAdmin)
	require.NoError(t, err)
	require.Len(t, admins, 1)
	assert.Equal(t, "list-admin@example.com", admins[0].User.Email)
}

func TestTenantUpdate_RenamesAndTransitions(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(context.Background())
	tenants := tenancy.NewTenantService(e.Stores, e.Tx)
	id := testutil.CreateTenant(t, tenant.WithID(ctx, tenant.DefaultID), e.Pool, "rename-me")

	name := "Renamed Tenancy"
	updated, err := tenants.Update(ctx, id, core.TenantUpdate{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, name, updated.Name)
	assert.Equal(t, core.TenantStatusActive, updated.Status)

	suspended := core.TenantStatusSuspended
	updated, err = tenants.Update(ctx, id, core.TenantUpdate{Status: &suspended})
	require.NoError(t, err)
	assert.Equal(t, core.TenantStatusSuspended, updated.Status)
	assert.Equal(t, name, updated.Name, "an absent name stays unchanged")
}
