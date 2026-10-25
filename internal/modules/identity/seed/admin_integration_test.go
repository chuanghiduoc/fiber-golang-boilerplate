//go:build integration

package seed_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/seed"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

const adminEmail = "admin@example.com"

func setup(t *testing.T) (pool *pgxpool.Pool, run func(cfg config.AdminConfig, tenantID int64) error) {
	t.Helper()
	pool, cleanup, err := testutil.SetupTestDB(context.Background())
	require.NoError(t, err)
	t.Cleanup(cleanup)
	stores := identityrepo.NewStores(testutil.Scoped(pool))
	tx := identityrepo.NewTransactor(testutil.NewTxManager(pool), nil, nil, nil)
	return pool, func(cfg config.AdminConfig, tenantID int64) error {
		return seed.Admin(context.Background(), cfg, stores, tx, tenantID)
	}
}

func adminCfg() config.AdminConfig {
	return config.AdminConfig{Email: " Admin@Example.com ", Password: "Admin123!", Name: "Root"}
}

func TestAdminCreatesVerifiedAdminMembershipOnce(t *testing.T) {
	pool, run := setup(t)
	ctx := context.Background()
	require.NoError(t, run(adminCfg(), 1))
	require.NoError(t, run(adminCfg(), 1), "the seed is idempotent")

	var (
		users, passwords, admins int
		verified                 bool
	)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), bool_and(email_verified_at IS NOT NULL) FROM users WHERE email = $1`, adminEmail).Scan(&users, &verified))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM user_passwords`).Scan(&passwords))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE tenant_id = 1 AND role = 'admin' AND status = 'active'`).Scan(&admins))
	assert.Equal(t, 1, users)
	assert.True(t, verified)
	assert.Equal(t, 1, passwords)
	assert.Equal(t, 1, admins)
}

func TestAdminRepairsUnverifiedExistingAdmin(t *testing.T) {
	pool, run := setup(t)
	ctx := context.Background()
	m := testutil.CreateMember(t, ctx, pool, 1, adminEmail, testutil.MemberOptions{Role: "admin", Unverified: true})
	require.NoError(t, run(adminCfg(), 1))
	var verified bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, m.UserID).Scan(&verified))
	assert.True(t, verified)
}

// The seed only creates: it never promotes, restores, or adds memberships to
// an account an operator already changed.
func TestAdminNeverPromotesOrJoins(t *testing.T) {
	cases := map[string]func(t *testing.T, pool *pgxpool.Pool){
		"demoted member": func(t *testing.T, pool *pgxpool.Pool) {
			testutil.CreateMember(t, context.Background(), pool, 1, adminEmail, testutil.MemberOptions{Role: "user"})
		},
		"member of another tenant only": func(t *testing.T, pool *pgxpool.Pool) {
			acme := testutil.CreateTenant(t, context.Background(), pool, "acme")
			testutil.CreateMember(t, context.Background(), pool, acme, adminEmail, testutil.MemberOptions{Role: "admin"})
		},
		"banned in the tenant": func(t *testing.T, pool *pgxpool.Pool) {
			testutil.CreateMember(t, context.Background(), pool, 1, "keeper@example.com", testutil.MemberOptions{Role: "admin"})
			m := testutil.CreateMember(t, context.Background(), pool, 1, adminEmail, testutil.MemberOptions{Role: "admin"})
			_, err := pool.Exec(context.Background(),
				`UPDATE memberships SET status = 'suspended', banned_at = NOW() WHERE tenant_id = 1 AND user_id = $1`, m.UserID)
			require.NoError(t, err)
			// The banned account deleted itself: the ban must still stop the seed.
			_, err = pool.Exec(context.Background(),
				`UPDATE users SET status = 'deleted', deleted_at = NOW() WHERE id = $1`, m.UserID)
			require.NoError(t, err)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			pool, run := setup(t)
			prepare(t, pool)
			require.NoError(t, run(adminCfg(), 1), "an operator decision never fails startup")
			var activeAdmin int
			require.NoError(t, pool.QueryRow(context.Background(), `
				SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id
				WHERE m.tenant_id = 1 AND u.email = $1 AND m.role = 'admin' AND m.status = 'active' AND u.status = 'active'`,
				adminEmail).Scan(&activeAdmin))
			assert.Zero(t, activeAdmin)
		})
	}
}

func TestAdminSkipsWithoutCredentials(t *testing.T) {
	pool, run := setup(t)
	require.NoError(t, run(config.AdminConfig{Email: adminEmail}, 1))
	var users int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users))
	assert.Zero(t, users)
}
