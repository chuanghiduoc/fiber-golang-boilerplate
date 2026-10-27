//go:build integration

package membership_test

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/internal/pgfixtures"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/tenancy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/google/uuid"
)

func TestPermissionSyncIsIdempotent(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	for i := 0; i < 2; i++ {
		require.NoError(t, membership.SyncPermissions(ctx, e.Tx, e.Stores.Roles, pgfixtures.Catalog))
	}

	permissions := scalar[int64](t, e.Pool, `SELECT count(*) FROM permissions`)
	assert.Equal(t, int64(len(pgfixtures.Catalog.AllPermissions())), permissions)

	counts := map[string]int64{}
	rows, err := e.Pool.Query(ctx, `
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

	assert.Equal(t, int64(len(pgfixtures.Catalog.AllTenantPermissions())), counts[authz.RoleOwner])
	assert.Equal(t, int64(len(pgfixtures.Catalog.AllTenantPermissions())), counts[authz.RoleAdmin])
	assert.Equal(t, int64(len(pgfixtures.Catalog.PermissionsForRole(authz.RoleMember))), counts[authz.RoleMember])
	assert.Equal(t, int64(len(pgfixtures.Catalog.AllPlatformPermissions())), counts[authz.RolePlatformAdmin])
}

func TestEffectivePermissionsAreUnionedInTheSessionState(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-member@example.com",
		testutil.MemberOptions{Role: "member"})
	_, err := e.Stores.Roles.Create(ctx, core.NewRole{
		Key: "auditor", Name: "Auditor", Permissions: []string{"invitations:read"},
	})
	require.NoError(t, err)
	require.NoError(t, e.Stores.Roles.ReplaceMemberRoles(ctx, member.UserID, []string{"member", "auditor"}))
	// A platform role assignment is global; the session's tenant stays
	// unchanged but the platform permission is part of its effective set.
	require.NoError(t, e.Stores.Roles.AssignPlatformRole(ctx, member.UserID, authz.RolePlatformAdmin))

	principal, err := e.Stores.Memberships.GetPrincipal(ctx, member.UserID)
	require.NoError(t, err)
	assert.True(t, principal.Platform)
	assert.Equal(t, core.RoleMember, principal.Role,
		"a tenant role must not become the compatibility role")
	assert.Contains(t, principal.Permissions, "invitations:read")
	assert.Contains(t, principal.Permissions, "profile:read")
	assert.Contains(t, principal.Permissions, string(authz.PermissionPlatformTenantsRead))
	assert.NotContains(t, principal.Permissions, string(authz.PermissionAdminUsersRead),
		"a tenant role must not grant an admin permission")

	pair, err := e.Sessions.Issue(ctx, member.UserID, 0, nil)
	require.NoError(t, err)
	state, err := e.Stores.Sessions.State(ctx, pair.SessionID, member.UserID)
	require.NoError(t, err)
	assert.Equal(t, core.RoleMember, state.Role)
	assert.True(t, state.Platform)
	assert.Contains(t, state.Permissions, "invitations:read")
	assert.Contains(t, state.Permissions, string(authz.PermissionPlatformTenantsRead))
}

func TestPlatformAdminVersusTenantAdmin(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	tenantAdmin := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-tenant-admin@example.com",
		testutil.MemberOptions{Role: "admin"})
	platformAdmin := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-platform@example.com",
		testutil.MemberOptions{Role: "member"})
	require.NoError(t, e.Stores.Roles.AssignPlatformRole(ctx, platformAdmin.UserID, authz.RolePlatformAdmin))

	admin, err := e.Stores.Memberships.GetPrincipal(ctx, tenantAdmin.UserID)
	require.NoError(t, err)
	assert.False(t, admin.Platform)
	assert.Contains(t, admin.Permissions, string(authz.PermissionAdminUsersRead))
	assert.NotContains(t, admin.Permissions, string(authz.PermissionPlatformTenantsRead),
		"a tenant admin is not a platform admin")

	platform, err := e.Stores.Memberships.GetPrincipal(ctx, platformAdmin.UserID)
	require.NoError(t, err)
	assert.True(t, platform.Platform)
	assert.Contains(t, platform.Permissions, string(authz.PermissionPlatformTenantsRead))
	assert.NotContains(t, platform.Permissions, string(authz.PermissionAdminUsersRead),
		"a platform role must not grant tenant administration")
}

func TestReplaceMemberRolesProtectsTheLastOwner(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	roles := membership.NewRoleService(e.Stores, e.Tx, nil, pgfixtures.Catalog)
	owner := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-owner@example.com",
		testutil.MemberOptions{Role: "owner"})

	_, err := roles.ReplaceMemberRoles(ctx, owner.UserID, []string{core.RoleMember})
	assert.Equal(t, string(identitycatalog.CodeUserLastAdmin), pgfixtures.AppCode(err))

	// Keeping the owner role while adding another role is a valid replacement.
	updated, err := roles.ReplaceMemberRoles(ctx, owner.UserID,
		[]string{core.RoleOwner, core.RoleMember})
	require.NoError(t, err)
	assert.Equal(t, core.RoleOwner, updated.Membership.Role)

	testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-owner-2@example.com",
		testutil.MemberOptions{Role: "owner"})
	updated, err = roles.ReplaceMemberRoles(ctx, owner.UserID, []string{core.RoleAdmin})
	require.NoError(t, err)
	assert.Equal(t, core.RoleAdmin, updated.Membership.Role)
}

func TestSyncCatalogPrunesPermissionHeldByAnAPIKey(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-stale@example.com",
		testutil.MemberOptions{})

	require.NoError(t, func() error {
		_, err := e.Pool.Exec(ctx,
			`INSERT INTO permissions (key, owner) VALUES ('legacy:removed', 'legacy')`)
		if err != nil {
			return err
		}
		var keyID uuid.UUID
		if err := e.Pool.QueryRow(ctx, `
			INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash, expires_at)
			VALUES ($2, $1, 'stale', 'p', decode(repeat('ab', 32), 'hex'), NOW() + interval '1 hour') RETURNING id`, member.UserID, tenant.DefaultID).Scan(&keyID); err != nil {
			return err
		}
		_, err = e.Pool.Exec(ctx,
			`INSERT INTO api_key_scopes (api_key_id, permission_key) VALUES ($1, 'legacy:removed')`, keyID)
		return err
	}())

	require.NoError(t, membership.SyncPermissions(ctx, e.Tx, e.Stores.Roles, pgfixtures.Catalog))
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM permissions WHERE key = 'legacy:removed'`))
	assert.Zero(t, scalar[int64](t, e.Pool,
		`SELECT count(*) FROM api_key_scopes WHERE permission_key = 'legacy:removed'`),
		"removing a catalog permission cascades to the key scopes (fail closed)")
}

func TestReplaceMemberRolesIsSerialized(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	roles := membership.NewRoleService(e.Stores, e.Tx, nil, pgfixtures.Catalog)
	member := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-concurrent@example.com",
		testutil.MemberOptions{})
	_, err := e.Stores.Roles.Create(ctx, core.NewRole{Key: "alpha", Name: "Alpha"})
	require.NoError(t, err)
	_, err = e.Stores.Roles.Create(ctx, core.NewRole{Key: "beta", Name: "Beta"})
	require.NoError(t, err)

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, key := range []string{"alpha", "beta"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = roles.ReplaceMemberRoles(ctx, member.UserID,
				[]string{core.RoleMember, key})
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	got, err := e.Stores.Memberships.Get(ctx, member.UserID)
	require.NoError(t, err)
	assert.Len(t, got.Membership.Roles, 2, "one replacement wins; roles are not unioned")
	assert.Contains(t, got.Membership.Roles, core.RoleMember)
	assert.False(t, slices.Contains(got.Membership.Roles, "alpha") && slices.Contains(got.Membership.Roles, "beta"))
}

func TestConcurrentLastOwnerRemovalLeavesOneOwner(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
	roles := membership.NewRoleService(e.Stores, e.Tx, nil, pgfixtures.Catalog)
	first := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-concurrent-owner-a@example.com",
		testutil.MemberOptions{Role: "owner"})
	second := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-concurrent-owner-b@example.com",
		testutil.MemberOptions{Role: "owner"})

	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i, id := range []uuid.UUID{first.UserID, second.UserID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = roles.ReplaceMemberRoles(ctx, id, []string{core.RoleMember})
		}()
	}
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case pgfixtures.AppCode(err) == string(identitycatalog.CodeUserLastAdmin):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, successes)
	assert.Equal(t, 1, conflicts)
	assert.Equal(t, int64(1), scalar[int64](t, e.Pool, `
		SELECT count(*) FROM membership_roles mr JOIN roles r ON r.id = mr.role_id
		WHERE mr.tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND r.tenant_id IS NULL AND r.key = 'owner'`))
}

func TestTenantRoleCannotShadowASystemRoleKey(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	roles := membership.NewRoleService(e.Stores, e.Tx, nil, pgfixtures.Catalog)
	_, err := roles.Create(authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID)), core.NewRole{Key: "owner", Name: "Fake owner"})
	assert.Equal(t, http.StatusConflict, pgfixtures.AppStatus(err))
}

func TestCreateTenantAssignsTheCallingPlatformAdminAsOwner(t *testing.T) {
	e := pgfixtures.NewEnv(t, nil)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	platformAdmin := testutil.CreateMember(t, ctx, e.Pool, tenant.DefaultID, "rbac-tenant-creator@example.com",
		testutil.MemberOptions{})

	tenants := tenancy.NewTenantService(e.Stores, e.Tx)
	created, err := tenants.Create(ctx, core.NewTenant{
		Slug: "rbac-created", Name: "Created", Status: core.TenantStatusActive,
		OwnerUserID: platformAdmin.UserID,
	})
	require.NoError(t, err)

	owner, err := e.Stores.Memberships.Get(tenant.WithID(ctx, created.ID), platformAdmin.UserID)
	require.NoError(t, err)
	assert.Equal(t, core.RoleOwner, owner.Membership.Role)
	assert.Equal(t, int64(1), scalar[int64](t, e.Pool,
		`SELECT count(*) FROM membership_roles WHERE tenant_id = $1 AND user_id = $2`,
		created.ID, platformAdmin.UserID))
}
