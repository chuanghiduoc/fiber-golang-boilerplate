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
)

// A status-only merge patch must obey the same hierarchy rule as a role
// change; otherwise an admin can suspend an owner or a peer admin.
func TestUpdateMember_StatusChangeRequiresStrictlyHigherRank(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "patch-owner@example.com",
		testutil.MemberOptions{Role: core.RoleOwner})
	admin := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "patch-admin@example.com",
		testutil.MemberOptions{Role: core.RoleAdmin})
	peer := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "patch-peer@example.com",
		testutil.MemberOptions{Role: core.RoleAdmin})
	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "patch-member@example.com",
		testutil.MemberOptions{Role: core.RoleMember})

	suspended := string(core.MembershipStatusSuspended)
	_, err := e.Admin.UpdateMember(memberContext(admin.UserID, core.RoleAdmin), owner.UserID,
		membership.MemberUpdate{Status: &suspended})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"an admin cannot suspend an owner through a status patch")
	assert.Equal(t, http.StatusForbidden, pgfixtures.AppStatus(err))

	_, err = e.Admin.UpdateMember(memberContext(admin.UserID, core.RoleAdmin), peer.UserID,
		membership.MemberUpdate{Status: &suspended})
	assert.Equal(t, string(identitycatalog.CodeRoleInsufficientRank), pgfixtures.AppCode(err),
		"an admin cannot suspend a peer admin through a status patch")

	stillActive, err := e.Stores.Memberships.Get(ctx, owner.UserID)
	require.NoError(t, err)
	assert.Equal(t, core.MembershipStatusActive, stillActive.Membership.Status,
		"the rejected patch must not change the target")

	updated, err := e.Admin.UpdateMember(memberContext(admin.UserID, core.RoleAdmin), member.UserID,
		membership.MemberUpdate{Status: &suspended})
	require.NoError(t, err, "an admin may still suspend a lower-ranked member")
	assert.Equal(t, core.MembershipStatusSuspended, updated.Membership.Status)
}
