//go:build integration

package database_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// TestNormalRoleCannotEnableBypass proves row-level security no longer depends
// on a setting the application role can write: turning app.rls_bypass on in a
// transaction still returns only the context tenant's rows.
func TestNormalRoleCannotEnableBypass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, cleanup, err := pgtest.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	tenantA, tenantB := insertFilesFixture(t, ctx, fixture)

	appPool := pgtest.AppPool(fixture)
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()

	_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.rls_bypass', 'on', true)`,
		fmt.Sprint(tenantA))
	require.NoError(t, err, "setting an unused GUC is allowed; it must not change isolation")

	var tenants []int64
	rows, err := tx.Query(ctx, `SELECT DISTINCT tenant_id FROM files ORDER BY tenant_id`)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		tenants = append(tenants, id)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Equal(t, []int64{tenantA}, tenants, "the foreign tenant must stay invisible")
	require.NotContains(t, tenants, tenantB)
}

// TestBypassContextUsesBypassRole proves both scoped statements and
// transactions marked with WithRLSBypass reach the dedicated BYPASSRLS role
// and read every tenant.
func TestBypassContextUsesBypassRole(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, cleanup, err := pgtest.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	tenantA, tenantB := insertFilesFixture(t, ctx, fixture)
	bypassCtx := pgtest.Bypass(ctx)

	var count int
	require.NoError(t, pgtest.Scoped(fixture).QueryRow(bypassCtx, `SELECT COUNT(DISTINCT tenant_id) FROM files`).Scan(&count))
	require.Equal(t, 2, count, "Scoped must use the bypass pool")

	count = 0
	require.NoError(t, pgtest.NewTxManager(fixture).WithTx(bypassCtx, func(tx pgx.Tx) error {
		return tx.QueryRow(bypassCtx, `SELECT COUNT(DISTINCT tenant_id) FROM files`).Scan(&count)
	}))
	require.Equal(t, 2, count, "TxManager must use the bypass pool")

	var user string
	require.NoError(t, pgtest.Scoped(fixture).QueryRow(bypassCtx, `SELECT current_user`).Scan(&user))
	require.Equal(t, pgtest.BypassRole, user, "cross-tenant work must run as the bypass role")

	// The normal pool keeps seeing the context tenant only, so the bypass
	// marker alone never widens a tenant-scoped request.
	count = 0
	require.NoError(t, pgtest.Scoped(fixture).QueryRow(tenant.WithID(ctx, tenantA), `SELECT COUNT(DISTINCT tenant_id) FROM files`).Scan(&count))
	require.Equal(t, 1, count)
	require.NotEqual(t, tenantB, tenantA)
}

// TestBypassInsideNormalTransactionStaysTenantScoped proves the pool is fixed
// at BEGIN: marking the context after a normal transaction began cannot move
// it to the bypass pool, so the statement keeps the transaction's isolation
// and sees no foreign rows (fail closed, documented on TxManager.WithTx).
func TestBypassInsideNormalTransactionStaysTenantScoped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, cleanup, err := pgtest.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	tenantA, tenantB := insertFilesFixture(t, ctx, fixture)

	err = pgtest.NewTxManager(fixture).WithTx(tenant.WithID(ctx, tenantA), func(tx pgx.Tx) error {
		var tenants []int64
		rows, err := tx.Query(database.WithRLSBypass(ctx), `SELECT DISTINCT tenant_id FROM files ORDER BY tenant_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			tenants = append(tenants, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		assert.Equal(t, []int64{tenantA}, tenants, "a bypass marker after BEGIN must not widen the transaction")
		assert.NotContains(t, tenants, tenantB)
		return nil
	})
	require.NoError(t, err)
}

// TestNormalRoleCannotSetRoleToBypass proves the normal role is not a member of
// the bypass role, so it cannot SET ROLE into it.
func TestNormalRoleCannotSetRoleToBypass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, cleanup, err := pgtest.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	appPool := pgtest.AppPool(fixture)

	conn, err := appPool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	var member bool
	require.NoError(t, conn.QueryRow(ctx, `SELECT pg_has_role(current_user, $1, 'MEMBER')`, pgtest.BypassRole).Scan(&member))
	require.False(t, member, "the normal role must not be a member of the bypass role")

	_, err = conn.Exec(ctx, `SET ROLE `+pgx.Identifier{pgtest.BypassRole}.Sanitize())
	require.Error(t, err, "SET ROLE to the bypass role must be refused")
	require.Contains(t, err.Error(), "permission denied")

	var current string
	require.NoError(t, conn.QueryRow(ctx, `SELECT current_user`).Scan(&current))
	require.Equal(t, pgtest.AppRole, current)
}

// insertFilesFixture creates two tenants with one file each through the
// privileged fixture pool (the bypass role) and returns their ids.
func insertFilesFixture(t *testing.T, ctx context.Context, fixture *pgxpool.Pool) (tenantA, tenantB int64) {
	t.Helper()
	tenantA = createTenantWithFile(t, ctx, fixture, "rls-roles-a", "a.txt")
	tenantB = createTenantWithFile(t, ctx, fixture, "rls-roles-b", "b.txt")
	return tenantA, tenantB
}

func createTenantWithFile(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug, path string) int64 {
	t.Helper()
	var tenantID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&tenantID))
	var userID int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (email, name, email_verified_at) VALUES ($1, $1, NOW()) RETURNING id`,
		slug+"@example.test").Scan(&userID))
	_, err := pool.Exec(ctx,
		`INSERT INTO memberships (tenant_id, user_id) VALUES ($1, $2)`, tenantID, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES ($1, $2, $3, $3, 'text/plain', 1)`, tenantID, userID, path)
	require.NoError(t, err)
	return tenantID
}
