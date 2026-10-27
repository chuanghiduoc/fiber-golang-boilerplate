//go:build integration

package database_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
)

// scopedLoops exceeds the test pool's MaxConns (5): a leaked connection makes
// a later Acquire block until the context deadline.
const scopedLoops = 12

func TestScopedReleasesConnectionsOnEveryPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fx := pgtest.New(t)
	pool := fx.Bypass

	other, err := pool.Exec(ctx, `INSERT INTO tenants (slug, name) VALUES ('scoped-b', 'B')`)
	require.NoError(t, err)
	require.Equal(t, int64(1), other.RowsAffected())
	_, err = pool.Exec(ctx, `INSERT INTO tenant_settings (tenant_id) SELECT id FROM tenants WHERE slug = 'scoped-b'`)
	require.NoError(t, err)

	appPool := fx.App
	scoped := database.NewScoped(appPool, database.WithResolver(func(context.Context) database.Scope {
		return database.Scope{TenantID: testID(1)}
	}))

	for i := range scopedLoops {
		// Only tenant 1 is visible to the app role.
		var count int
		require.NoError(t, scoped.QueryRow(ctx, `SELECT count(*) FROM tenant_settings`).Scan(&count), "loop %d", i)
		assert.Equal(t, 1, count)

		// Err mid-iteration, then Close.
		rows, err := scoped.Query(ctx, `SELECT generate_series(1, 3)`)
		require.NoError(t, err)
		require.True(t, rows.Next())
		require.NoError(t, rows.Err())
		rows.Close()

		// A failing statement must not poison or leak its connection.
		_, err = scoped.Exec(ctx, `SELECT 1 / 0`)
		require.Error(t, err)

		var none int
		err = scoped.QueryRow(ctx, `SELECT 1 WHERE false`).Scan(&none)
		require.True(t, errors.Is(err, pgx.ErrNoRows), "QueryRow on no rows = %v", err)
	}
	assert.LessOrEqual(t, appPool.Stat().AcquiredConns(), int32(0), "every connection is back in the pool")
}

// TestScopedCommitErrorReachesCaller proves a COMMIT that fails after the
// statement itself succeeded (a deferred constraint is only checked at COMMIT)
// is reported by QueryRow.Scan, Query rows.Err, and Exec. Without the error
// check a failed INSERT ... RETURNING would look successful.
func TestScopedCommitErrorReachesCaller(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping provider COMMIT failure test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	require.NoError(t, admin.Ping(ctx))

	schema := fmt.Sprintf("scope_commit_%d", time.Now().UnixNano())
	ident := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+ident)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+ident+" CASCADE")
	})

	_, err = admin.Exec(ctx, "CREATE TABLE "+ident+".parents (id bigint PRIMARY KEY)")
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `
		CREATE TABLE `+ident+`.children (
			id bigint PRIMARY KEY,
			parent_id bigint NOT NULL,
			CONSTRAINT children_parent_fk FOREIGN KEY (parent_id)
				REFERENCES `+ident+`.parents (id) DEFERRABLE INITIALLY DEFERRED
		)`)
	require.NoError(t, err)

	pools, err := pgtest.OpenAppPools(ctx, dsn, schema)
	require.NoError(t, err)
	t.Cleanup(pools.Close)

	scoped := database.NewScoped(pools.Primary, database.WithResolver(func(context.Context) database.Scope {
		return database.Scope{TenantID: testID(1)}
	}))

	t.Run("QueryRow Scan", func(t *testing.T) {
		var id int64
		err := scoped.QueryRow(ctx,
			`INSERT INTO children (id, parent_id) VALUES (1, 999) RETURNING id`,
		).Scan(&id)
		require.Error(t, err, "a deferred foreign-key violation must surface as a COMMIT error")
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23503", pgErr.Code)
	})

	t.Run("Query rows.Err", func(t *testing.T) {
		rows, err := scoped.Query(ctx,
			`INSERT INTO children (id, parent_id) VALUES (2, 999) RETURNING id`)
		require.NoError(t, err)
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23503", pgErr.Code)
	})

	t.Run("Exec", func(t *testing.T) {
		_, err := scoped.Exec(ctx,
			`INSERT INTO children (id, parent_id) VALUES (3, 999) RETURNING id`)
		require.Error(t, err)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23503", pgErr.Code)
	})
}
