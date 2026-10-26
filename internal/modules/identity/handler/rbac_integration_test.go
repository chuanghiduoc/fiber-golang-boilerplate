//go:build integration

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

func TestIntegration_RolesAndPlatformTenants(t *testing.T) {
	h := newIdentityHarness(t, nil)
	app := identityApp(h)
	ctx := context.Background()
	const pw = "Password1!"

	owner := testutil.CreateMember(t, ctx, h.pool, 1, "rbac-owner@example.com",
		testutil.MemberOptions{Role: "owner", Password: pw})
	admin := testutil.CreateMember(t, ctx, h.pool, 1, "rbac-admin@example.com",
		testutil.MemberOptions{Role: "admin", Password: pw})
	member := testutil.CreateMember(t, ctx, h.pool, 1, "rbac-member@example.com",
		testutil.MemberOptions{Role: "member", Password: pw})
	platform := testutil.CreateMember(t, ctx, h.pool, 1, "rbac-platform@example.com",
		testutil.MemberOptions{Role: "member", Password: pw})
	require.NoError(t, h.stores.Roles.AssignPlatformRole(ctx, platform.UserID, authz.RolePlatformAdmin))

	ownerLogin := mustLogin(t, app, "default", owner.Email, pw)
	adminLogin := mustLogin(t, app, "default", admin.Email, pw)
	memberLogin := mustLogin(t, app, "default", member.Email, pw)
	platformLogin := mustLogin(t, app, "default", platform.Email, pw)
	// Platform routes require a second factor: a password-only
	// session is refused, an otp session is allowed.
	platformMFA := testutil.SessionTokenAMR(t, ctx, h.pool, platform, harnessSecret, []string{"pwd", "otp"})

	// Creating a role requires admin:roles:write; a plain member is denied.
	roleBody := dto.CreateRoleRequest{Key: "auditor", Name: "Auditor", Permissions: []string{"invitations:read"}}
	status, _ := do(t, app, call{method: http.MethodPost, path: "/api/v1/admin/roles/", tenant: "default",
		bearer: memberLogin.AccessToken, body: roleBody})
	assert.Equal(t, http.StatusForbidden, status)

	status, raw := do(t, app, call{method: http.MethodPost, path: "/api/v1/admin/roles/", tenant: "default",
		bearer: ownerLogin.AccessToken, body: roleBody})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var role dto.RoleResponse
	require.NoError(t, json.Unmarshal(raw, &role))
	assert.Equal(t, "auditor", role.Key)
	assert.False(t, role.System)

	// System role keys cannot be shadowed.
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/admin/roles/", tenant: "default",
		bearer: ownerLogin.AccessToken, body: dto.CreateRoleRequest{Key: "owner", Name: "Fake"}})
	assert.Equal(t, http.StatusConflict, status)

	// Tenant admins (without platform assignment) cannot use platform routes.
	status, _ = do(t, app, call{method: http.MethodPost, path: "/api/v1/platform/tenants/", tenant: "default",
		bearer: adminLogin.AccessToken, body: dto.CreateTenantRequest{Slug: "denied", Name: "Denied"}})
	assert.Equal(t, http.StatusForbidden, status)

	status, raw = do(t, app, call{method: http.MethodGet, path: "/api/v1/platform/tenants", tenant: "default",
		bearer: platformLogin.AccessToken})
	assert.Equal(t, http.StatusForbidden, status, string(raw))
	assert.Contains(t, string(raw), "auth.mfa_required")

	// A platform admin creates a tenant and becomes its owner.
	status, raw = do(t, app, call{method: http.MethodPost, path: "/api/v1/platform/tenants/", tenant: "default",
		bearer: platformMFA, body: dto.CreateTenantRequest{Slug: "rbac-acme", Name: "RBAC Acme"}})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created dto.TenantResponse
	require.NoError(t, json.Unmarshal(raw, &created))
	assert.Equal(t, "active", created.Status)

	ownerMember, err := h.stores.Memberships.Get(tenant.WithID(ctx, created.ID), platform.UserID)
	require.NoError(t, err)
	assert.Equal(t, "owner", ownerMember.Membership.Role)

	status, raw = do(t, app, call{method: http.MethodGet, path: "/api/v1/platform/tenants", tenant: "default",
		bearer: platformMFA})
	require.Equal(t, http.StatusOK, status, string(raw))
	status, _ = do(t, app, call{method: http.MethodGet,
		path: "/api/v1/platform/tenants/" + strconv.FormatInt(created.ID, 10), tenant: "default",
		bearer: platformMFA})
	assert.Equal(t, http.StatusOK, status)
	status, _ = do(t, app, call{method: http.MethodPatch,
		path: "/api/v1/platform/tenants/" + strconv.FormatInt(created.ID, 10) + "/status", tenant: "default",
		bearer: platformMFA, body: dto.UpdateTenantStatusRequest{Status: "suspended"}})
	assert.Equal(t, http.StatusOK, status)

	// Replace the member's roles with the tenant role; the compatibility role
	// stays the highest system role.
	status, raw = do(t, app, call{method: http.MethodPut,
		path: "/api/v1/admin/users/" + strconv.FormatInt(member.UserID, 10) + "/roles", tenant: "default",
		bearer: ownerLogin.AccessToken, body: dto.ReplaceMemberRolesRequest{Roles: []string{"auditor"}}})
	require.Equal(t, http.StatusOK, status, string(raw))
	var updated dto.UserResponse
	require.NoError(t, json.Unmarshal(raw, &updated))
	assert.Equal(t, "member", updated.Role)
	assert.Contains(t, updated.Roles, "auditor")

	// An assigned role cannot be deleted; once detached it can.
	status, raw = do(t, app, call{method: http.MethodDelete,
		path: "/api/v1/admin/roles/" + strconv.FormatInt(role.ID, 10), tenant: "default",
		bearer: ownerLogin.AccessToken})
	assert.Equal(t, http.StatusConflict, status, string(raw))
	status, _ = do(t, app, call{method: http.MethodPut,
		path: "/api/v1/admin/users/" + strconv.FormatInt(member.UserID, 10) + "/roles", tenant: "default",
		bearer: ownerLogin.AccessToken, body: dto.ReplaceMemberRolesRequest{Roles: []string{"member"}}})
	require.Equal(t, http.StatusOK, status)
	status, _ = do(t, app, call{method: http.MethodDelete,
		path: "/api/v1/admin/roles/" + strconv.FormatInt(role.ID, 10), tenant: "default",
		bearer: ownerLogin.AccessToken})
	assert.Equal(t, http.StatusNoContent, status)
}
