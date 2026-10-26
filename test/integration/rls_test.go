//go:build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
)

// TestRLSTenantIsolation proves row-level security is the second line behind
// the application's tenant predicates: the test pool connects as
// the non-superuser app role, and every assertion runs through the same
// database.Scoped handle the API uses.
func TestRLSTenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	tenantA := testutil.CreateTenant(t, ctx, pool, "rls-a")
	tenantB := testutil.CreateTenant(t, ctx, pool, "rls-b")
	userA := testutil.CreateMember(t, ctx, pool, tenantA, "rls-a@example.test", testutil.MemberOptions{})
	userB := testutil.CreateMember(t, ctx, pool, tenantB, "rls-b@example.test", testutil.MemberOptions{})

	var unsafe bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&unsafe))
	require.False(t, unsafe, "integration tests must run as a role RLS applies to")

	scoped := testutil.Scoped(pool)
	insert := func(tenantID, userID int64, path string) {
		t.Helper()
		_, err := scoped.Exec(tenant.WithID(ctx, tenantID),
			`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
			 VALUES ($1, $2, $3, $3, 'text/plain', 1)`,
			tenantID, userID, path)
		require.NoError(t, err)
	}
	insert(tenantA, userA.UserID, "a.txt")
	insert(tenantB, userB.UserID, "b.txt")

	// A query with no tenant predicate sees only the context tenant.
	ctxA := tenant.WithID(ctx, tenantA)
	rows, err := scoped.Query(ctxA, `SELECT storage_path FROM files ORDER BY storage_path`)
	require.NoError(t, err)
	var paths []string
	for rows.Next() {
		var path string
		require.NoError(t, rows.Scan(&path))
		paths = append(paths, path)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Equal(t, []string{"a.txt"}, paths)

	// A transaction gets the same scope from its context.
	require.NoError(t, testutil.NewTxManager(pool).WithTx(ctxA, func(tx pgx.Tx) error {
		var count int
		require.NoError(t, tx.QueryRow(ctxA, `SELECT COUNT(*) FROM files`).Scan(&count))
		require.Equal(t, 1, count)
		return nil
	}))

	// A write for another tenant is rejected by WITH CHECK.
	ctxB := tenant.WithID(ctx, tenantB)
	_, err = scoped.Exec(ctxB,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES ($1, $2, 'cross.txt', 'cross.txt', 'text/plain', 1)`,
		tenantA, userA.UserID)
	require.Error(t, err, "WITH CHECK must reject another tenant's row")

	// No setting at all sees nothing: reset the pool's fixture default on one
	// dedicated connection (and restore it before releasing the connection).
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	_, err = conn.Exec(ctx, `RESET app.tenant_id`)
	require.NoError(t, err)
	_, err = conn.Exec(ctx, `RESET app.rls_bypass`)
	require.NoError(t, err)
	var unset int
	require.NoError(t, conn.QueryRow(ctx, `SELECT COUNT(*) FROM files`).Scan(&unset))
	require.Zero(t, unset, "a connection without a tenant must see no rows")
	_, err = conn.Exec(ctx, `SELECT set_config('app.rls_bypass', 'on', false)`)
	require.NoError(t, err)

	// Bypass sees every tenant, for the deliberate cross-tenant job paths.
	bypass := testutil.BypassScoped(pool)
	var all int
	require.NoError(t, bypass.QueryRow(ctx, `SELECT COUNT(*) FROM files`).Scan(&all))
	require.Equal(t, 2, all)
}
