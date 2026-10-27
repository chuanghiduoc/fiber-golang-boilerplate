//go:build integration

package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

func TestUpdateMember_AppliesRolesAndStatusAndChecksETag(t *testing.T) {
	e := newEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "member-owner@example.com", testutil.MemberOptions{Role: service.RoleOwner})
	target := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "member-target@example.com", testutil.MemberOptions{Role: service.RoleMember})

	current, err := e.admin.GetMember(ctx, target.UserID)
	require.NoError(t, err)
	expected := current.UpdatedAt()

	status := string(service.MembershipStatusSuspended)
	updated, err := e.admin.UpdateMember(ctx, target.UserID, service.MemberUpdate{
		ExpectedUpdatedAt: expected,
		RoleKeys:          []string{service.RoleAdmin},
		Status:            &status,
	})
	require.NoError(t, err)
	assert.Equal(t, string(service.MembershipStatusSuspended), string(updated.Membership.Status))
	assert.NotNil(t, updated.Membership.BannedAt)
	assert.Equal(t, service.RoleAdmin, updated.Membership.Role)

	// The validator read before the mutation is now stale.
	_, err = e.admin.UpdateMember(ctx, target.UserID, service.MemberUpdate{
		ExpectedUpdatedAt: expected,
		Status:            &status,
	})
	assert.Equal(t, 412, appStatus(err))

	active := string(service.MembershipStatusActive)
	restored, err := e.admin.UpdateMember(ctx, target.UserID, service.MemberUpdate{Status: &active})
	require.NoError(t, err)
	assert.Equal(t, string(service.MembershipStatusActive), string(restored.Membership.Status))
	assert.Nil(t, restored.Membership.BannedAt)

	removed := string(service.MembershipStatusRemoved)
	_, err = e.admin.UpdateMember(ctx, target.UserID, service.MemberUpdate{Status: &removed})
	require.NoError(t, err)
	gone, err := e.admin.GetMember(ctx, target.UserID)
	require.NoError(t, err)
	assert.Equal(t, string(service.MembershipStatusRemoved), string(gone.Membership.Status))
}

func TestListMembers_FiltersByStatusAndRole(t *testing.T) {
	e := newEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "list-owner@example.com", testutil.MemberOptions{Role: service.RoleOwner})
	testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "list-admin@example.com", testutil.MemberOptions{Role: service.RoleAdmin})
	member := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "list-member@example.com", testutil.MemberOptions{Role: service.RoleMember})
	require.NoError(t, e.admin.BanUser(ctx, member.UserID))

	page, hasMore, err := e.admin.ListMembers(ctx, 100, "", string(service.MembershipStatusSuspended), service.RoleMember)
	require.NoError(t, err)
	assert.False(t, hasMore)
	require.Len(t, page, 1)
	assert.Equal(t, member.UserID, page[0].User.ID)

	admins, _, err := e.admin.ListMembers(ctx, 100, "", string(service.MembershipStatusActive), service.RoleAdmin)
	require.NoError(t, err)
	require.Len(t, admins, 1)
	assert.Equal(t, "list-admin@example.com", admins[0].User.Email)
}

func TestTenantUpdate_RenamesAndTransitions(t *testing.T) {
	e := newEnv(t, nil)
	ctx := authz.SystemContext(context.Background())
	tenants := service.NewTenantService(e.stores, e.tx)
	id := testutil.CreateTenant(t, tenant.WithID(ctx, tenant.DefaultID), e.pool, "rename-me")

	name := "Renamed Tenancy"
	updated, err := tenants.Update(ctx, id, service.TenantUpdate{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, name, updated.Name)
	assert.Equal(t, service.TenantStatusActive, updated.Status)

	suspended := service.TenantStatusSuspended
	updated, err = tenants.Update(ctx, id, service.TenantUpdate{Status: &suspended})
	require.NoError(t, err)
	assert.Equal(t, service.TenantStatusSuspended, updated.Status)
	assert.Equal(t, name, updated.Name, "an absent name stays unchanged")
}
