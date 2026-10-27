//go:build integration

package seed_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/seed"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/google/uuid"
)

const adminEmail = "admin@example.com"

func setup(t *testing.T) (fx *pgtest.Fixture, run func(cfg config.AdminConfig, tenantID uuid.UUID) error) {
	t.Helper()
	fx = testutil.NewFixture(t)
	stores := identityrepo.NewStores(fx.Scoped(), testutil.BanHasher(t))
	tx := identityrepo.NewTransactor(fx.Tx(), nil, nil, nil, testutil.BanHasher(t))
	return fx, func(cfg config.AdminConfig, tenantID uuid.UUID) error {
		return seed.Admin(context.Background(), cfg, stores, tx, tenantID)
	}
}

func adminCfg() config.AdminConfig {
	return config.AdminConfig{Email: " Admin@Example.com ", Password: strings.Join([]string{"seed", "fixture", "credential"}, "-") + "-2026", Name: "Root"}
}

func TestAdminCreatesVerifiedOwnerMembershipOnce(t *testing.T) {
	fx, run := setup(t)
	pool := fx.Bypass
	ctx := context.Background()
	require.NoError(t, run(adminCfg(), tenant.DefaultID))
	require.NoError(t, run(adminCfg(), tenant.DefaultID), "the seed is idempotent")

	var (
		users, passwords, owners, platformAdmins int
		verified                                 bool
	)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), bool_and(email_verified_at IS NOT NULL) FROM users WHERE email = $1`, adminEmail).Scan(&users, &verified))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM user_passwords`).Scan(&passwords))
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM memberships m
		JOIN membership_roles mr ON mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
		JOIN roles r ON r.id = mr.role_id
		WHERE m.tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND m.status = 'active'
		  AND r.tenant_id IS NULL AND r.key = 'owner'`).Scan(&owners))
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM platform_role_assignments pra
		JOIN roles r ON r.id = pra.role_id
		JOIN users u ON u.id = pra.user_id
		WHERE u.email = $1 AND r.scope = 'platform' AND r.key = 'platform_admin'`, adminEmail).Scan(&platformAdmins))
	assert.Equal(t, 1, users)
	assert.True(t, verified)
	assert.Equal(t, 1, passwords)
	assert.Equal(t, 1, owners)
	assert.Equal(t, 1, platformAdmins)
}

func TestAdminRepairsUnverifiedExistingOwner(t *testing.T) {
	fx, run := setup(t)
	pool := fx.Bypass
	ctx := context.Background()
	m := testutil.CreateMember(t, ctx, pool, tenant.DefaultID, adminEmail, testutil.MemberOptions{Role: "owner", Unverified: true})
	require.NoError(t, run(adminCfg(), tenant.DefaultID))
	var verified bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, m.UserID).Scan(&verified))
	assert.True(t, verified)
}

// The seed only creates: it never promotes, restores, or adds memberships to
// an account an operator already changed.
func TestAdminNeverPromotesOrJoins(t *testing.T) {
	cases := map[string]func(t *testing.T, pool *pgxpool.Pool){
		"demoted member": func(t *testing.T, pool *pgxpool.Pool) {
			testutil.CreateMember(t, context.Background(), pool, tenant.DefaultID, adminEmail, testutil.MemberOptions{Role: "member"})
		},
		"member of another tenant only": func(t *testing.T, pool *pgxpool.Pool) {
			acme := testutil.CreateTenant(t, context.Background(), pool, "acme")
			testutil.CreateMember(t, context.Background(), pool, acme, adminEmail, testutil.MemberOptions{Role: "owner"})
		},
		"banned in the tenant": func(t *testing.T, pool *pgxpool.Pool) {
			testutil.CreateMember(t, context.Background(), pool, tenant.DefaultID, "keeper@example.com", testutil.MemberOptions{Role: "owner"})
			m := testutil.CreateMember(t, context.Background(), pool, tenant.DefaultID, adminEmail, testutil.MemberOptions{Role: "owner"})
			hash, kid := testutil.BanHasher(t).Marker(m.Email)
			_, err := pool.Exec(context.Background(),
				`UPDATE memberships SET status = 'suspended', banned_at = NOW(),
				        banned_email_hash = $2, banned_email_hash_kid = $3
				 WHERE tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND user_id = $1`, m.UserID, hash, kid)
			require.NoError(t, err)
			// The banned account deleted itself: the ban must still stop the seed.
			_, err = pool.Exec(context.Background(),
				`UPDATE users SET status = 'deleted', deleted_at = NOW() WHERE id = $1`, m.UserID)
			require.NoError(t, err)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			fx, run := setup(t)
			pool := fx.Bypass
			prepare(t, pool)
			require.NoError(t, run(adminCfg(), tenant.DefaultID), "an operator decision never fails startup")
			var activeOwner int
			require.NoError(t, pool.QueryRow(context.Background(), `
				SELECT count(*) FROM memberships m
				JOIN users u ON u.id = m.user_id
				JOIN membership_roles mr ON mr.tenant_id = m.tenant_id AND mr.user_id = m.user_id
				JOIN roles r ON r.id = mr.role_id
				WHERE m.tenant_id = '0192c9b0-7e4a-7000-8000-000000000001' AND u.email = $1 AND m.status = 'active' AND u.status = 'active'
				  AND r.tenant_id IS NULL AND r.key = 'owner'`,
				adminEmail).Scan(&activeOwner))
			assert.Zero(t, activeOwner)
		})
	}
}

func TestAdminSkipsWithoutCredentials(t *testing.T) {
	fx, run := setup(t)
	pool := fx.Bypass
	require.NoError(t, run(config.AdminConfig{Email: adminEmail}, tenant.DefaultID))
	var users int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users))
	assert.Zero(t, users)
}
