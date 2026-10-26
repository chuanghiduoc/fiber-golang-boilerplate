//go:build integration

package service_test

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestPermissionSyncIsIdempotent(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		require.NoError(t, identityservice.SyncPermissions(ctx, e.tx, e.stores.Roles))
	}

	permissions := scalar[int64](t, e.pool, `SELECT count(*) FROM permissions`)
	assert.Equal(t, int64(len(authz.AllPermissions())), permissions)

	counts := map[string]int64{}
	rows, err := e.pool.Query(ctx, `
		SELECT r.key, count(rp.permission_key)
		FROM roles r LEFT JOIN role_permissions rp ON rp.role_id = r.id
		WHERE r.tenant_id IS NULL
		GROUP BY r.key`)
	require.NoError(t, err)
	for rows.Next() {
		var key string
		var n int64
		require.NoError(t, rows.Scan(&key, &n))
		counts[key] = n
	}
	require.NoError(t, rows.Err())

	assert.Equal(t, int64(len(authz.AllTenantPermissions())), counts[authz.RoleOwner])
	assert.Equal(t, int64(len(authz.AllTenantPermissions())), counts[authz.RoleAdmin])
	assert.Equal(t, int64(len(authz.PermissionsForRole(authz.RoleMember))), counts[authz.RoleMember])
	assert.Equal(t, int64(len(authz.AllPlatformPermissions())), counts[authz.RolePlatformAdmin])
}

func TestEffectivePermissionsAreUnionedInTheSessionState(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	member := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-member@example.com",
		testutil.MemberOptions{Role: "member"})
	_, err := e.stores.Roles.Create(ctx, identityservice.NewRole{
		Key: "auditor", Name: "Auditor", Permissions: []string{"invitations:read"},
	})
	require.NoError(t, err)
	require.NoError(t, e.stores.Roles.ReplaceMemberRoles(ctx, member.UserID, []string{"member", "auditor"}))
	// A platform role assignment is global; the session's tenant stays
	// unchanged but the platform permission is part of its effective set.
	require.NoError(t, e.stores.Roles.AssignPlatformRole(ctx, member.UserID, authz.RolePlatformAdmin))

	principal, err := e.stores.Memberships.GetPrincipal(ctx, member.UserID)
	require.NoError(t, err)
	assert.True(t, principal.Platform)
	assert.Equal(t, identityservice.RoleMember, principal.Role,
		"a tenant role must not become the compatibility role")
	assert.Contains(t, principal.Permissions, "invitations:read")
	assert.Contains(t, principal.Permissions, "profile:read")
	assert.Contains(t, principal.Permissions, string(authz.PermissionPlatformTenantsRead))
	assert.NotContains(t, principal.Permissions, string(authz.PermissionAdminUsersRead),
		"a tenant role must not grant an admin permission")

	pair, err := e.sessions.Issue(ctx, member.UserID, 0, nil)
	require.NoError(t, err)
	state, err := e.stores.Sessions.State(ctx, pair.SessionID, member.UserID)
	require.NoError(t, err)
	assert.Equal(t, identityservice.RoleMember, state.Role)
	assert.True(t, state.Platform)
	assert.Contains(t, state.Permissions, "invitations:read")
	assert.Contains(t, state.Permissions, string(authz.PermissionPlatformTenantsRead))
}

func TestPlatformAdminVersusTenantAdmin(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()

	tenantAdmin := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-tenant-admin@example.com",
		testutil.MemberOptions{Role: "admin"})
	platformAdmin := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-platform@example.com",
		testutil.MemberOptions{Role: "member"})
	require.NoError(t, e.stores.Roles.AssignPlatformRole(ctx, platformAdmin.UserID, authz.RolePlatformAdmin))

	admin, err := e.stores.Memberships.GetPrincipal(ctx, tenantAdmin.UserID)
	require.NoError(t, err)
	assert.False(t, admin.Platform)
	assert.Contains(t, admin.Permissions, string(authz.PermissionAdminUsersRead))
	assert.NotContains(t, admin.Permissions, string(authz.PermissionPlatformTenantsRead),
		"a tenant admin is not a platform admin")

	platform, err := e.stores.Memberships.GetPrincipal(ctx, platformAdmin.UserID)
	require.NoError(t, err)
	assert.True(t, platform.Platform)
	assert.Contains(t, platform.Permissions, string(authz.PermissionPlatformTenantsRead))
	assert.NotContains(t, platform.Permissions, string(authz.PermissionAdminUsersRead),
		"a platform role must not grant tenant administration")
}

func TestReplaceMemberRolesProtectsTheLastOwner(t *testing.T) {
	e := newEnv(t, nil)
	ctx := authz.SystemContext(context.Background())
	roles := identityservice.NewRoleService(e.stores, e.tx, nil)
	owner := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-owner@example.com",
		testutil.MemberOptions{Role: "owner"})

	_, err := roles.ReplaceMemberRoles(ctx, owner.UserID, []string{identityservice.RoleMember})
	assert.Equal(t, string(apperror.CodeUserLastAdmin), appCode(err))

	// Keeping the owner role while adding another role is a valid replacement.
	updated, err := roles.ReplaceMemberRoles(ctx, owner.UserID,
		[]string{identityservice.RoleOwner, identityservice.RoleMember})
	require.NoError(t, err)
	assert.Equal(t, identityservice.RoleOwner, updated.Membership.Role)

	testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-owner-2@example.com",
		testutil.MemberOptions{Role: "owner"})
	updated, err = roles.ReplaceMemberRoles(ctx, owner.UserID, []string{identityservice.RoleAdmin})
	require.NoError(t, err)
	assert.Equal(t, identityservice.RoleAdmin, updated.Membership.Role)
}

func TestSyncCatalogPrunesPermissionHeldByAnAPIKey(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	member := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-stale@example.com",
		testutil.MemberOptions{})

	require.NoError(t, func() error {
		_, err := e.pool.Exec(ctx,
			`INSERT INTO permissions (key, owner) VALUES ('legacy:removed', 'legacy')`)
		if err != nil {
			return err
		}
		var keyID int64
		if err := e.pool.QueryRow(ctx, `
			INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash)
			VALUES (1, $1, 'stale', 'p', 'rbac-stale-hash') RETURNING id`, member.UserID).Scan(&keyID); err != nil {
			return err
		}
		_, err = e.pool.Exec(ctx,
			`INSERT INTO api_key_scopes (api_key_id, permission_key) VALUES ($1, 'legacy:removed')`, keyID)
		return err
	}())

	require.NoError(t, identityservice.SyncPermissions(ctx, e.tx, e.stores.Roles))
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM permissions WHERE key = 'legacy:removed'`))
	assert.Zero(t, scalar[int64](t, e.pool,
		`SELECT count(*) FROM api_key_scopes WHERE permission_key = 'legacy:removed'`),
		"removing a catalog permission cascades to the key scopes (fail closed)")
}

func TestReplaceMemberRolesIsSerialized(t *testing.T) {
	e := newEnv(t, nil)
	ctx := authz.SystemContext(context.Background())
	roles := identityservice.NewRoleService(e.stores, e.tx, nil)
	member := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-concurrent@example.com",
		testutil.MemberOptions{})
	_, err := e.stores.Roles.Create(ctx, identityservice.NewRole{Key: "alpha", Name: "Alpha"})
	require.NoError(t, err)
	_, err = e.stores.Roles.Create(ctx, identityservice.NewRole{Key: "beta", Name: "Beta"})
	require.NoError(t, err)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, key := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = roles.ReplaceMemberRoles(ctx, member.UserID,
				[]string{identityservice.RoleMember, key})
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	got, err := e.stores.Memberships.Get(ctx, member.UserID)
	require.NoError(t, err)
	assert.Len(t, got.Membership.Roles, 2, "one replacement wins; roles are not unioned")
	assert.Contains(t, got.Membership.Roles, identityservice.RoleMember)
	assert.False(t, slices.Contains(got.Membership.Roles, "alpha") && slices.Contains(got.Membership.Roles, "beta"))
}

func TestConcurrentLastOwnerRemovalLeavesOneOwner(t *testing.T) {
	e := newEnv(t, nil)
	ctx := authz.SystemContext(context.Background())
	roles := identityservice.NewRoleService(e.stores, e.tx, nil)
	first := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-concurrent-owner-a@example.com",
		testutil.MemberOptions{Role: "owner"})
	second := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-concurrent-owner-b@example.com",
		testutil.MemberOptions{Role: "owner"})

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []int64{first.UserID, second.UserID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = roles.ReplaceMemberRoles(ctx, id, []string{identityservice.RoleMember})
		}()
	}
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case appCode(err) == string(apperror.CodeUserLastAdmin):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
	assert.Equal(t, int64(1), scalar[int64](t, e.pool, `
		SELECT count(*) FROM membership_roles mr JOIN roles r ON r.id = mr.role_id
		WHERE mr.tenant_id = 1 AND r.tenant_id IS NULL AND r.key = 'owner'`))
}

func TestTenantRoleCannotShadowASystemRoleKey(t *testing.T) {
	e := newEnv(t, nil)
	roles := identityservice.NewRoleService(e.stores, e.tx, nil)
	_, err := roles.Create(authz.SystemContext(context.Background()), identityservice.NewRole{Key: "owner", Name: "Fake owner"})
	assert.Equal(t, http.StatusConflict, appStatus(err))
}

func TestCreateTenantAssignsTheCallingPlatformAdminAsOwner(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	platformAdmin := testutil.CreateMember(t, ctx, e.pool, tenant.DefaultID, "rbac-tenant-creator@example.com",
		testutil.MemberOptions{})

	tenants := identityservice.NewTenantService(e.stores, e.tx)
	created, err := tenants.Create(ctx, identityservice.NewTenant{
		Slug: "rbac-created", Name: "Created", Status: identityservice.TenantStatusActive,
		OwnerUserID: platformAdmin.UserID,
	})
	require.NoError(t, err)

	owner, err := e.stores.Memberships.Get(tenant.WithID(ctx, created.ID), platformAdmin.UserID)
	require.NoError(t, err)
	assert.Equal(t, identityservice.RoleOwner, owner.Membership.Role)
	assert.Equal(t, int64(1), scalar[int64](t, e.pool,
		`SELECT count(*) FROM membership_roles WHERE tenant_id = $1 AND user_id = $2`,
		created.ID, platformAdmin.UserID))
}
