//go:build integration

package database_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// scopedLoops exceeds the test pool's MaxConns (5): a leaked connection makes
// a later Acquire block until the context deadline.
const scopedLoops = 12

func TestScopedReleasesConnectionsOnEveryPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := pgtest.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	other, err := pool.Exec(ctx, `INSERT INTO tenants (slug, name) VALUES ('scoped-b', 'B')`)
	require.NoError(t, err)
	require.Equal(t, int64(1), other.RowsAffected())
	_, err = pool.Exec(ctx, `INSERT INTO tenant_settings (tenant_id) SELECT id FROM tenants WHERE slug = 'scoped-b'`)
	require.NoError(t, err)

	scoped := database.NewScoped(pool, database.WithResolver(func(context.Context) database.Scope {
		return database.Scope{TenantID: 1}
	}))

	for i := range scopedLoops {
		// The session default bypass is overridden: only tenant 1 is visible.
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
	assert.LessOrEqual(t, pool.Stat().AcquiredConns(), int32(0), "every connection is back in the pool")
}
