package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/fakedb"
)

func TestPurgeTenantDeletesNonRunningJobsInBoundedBatches(t *testing.T) {
	db := &fakedb.DB{}
	db.ExecFn = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		if !strings.Contains(sql, "DELETE FROM jobs") {
			return pgconn.CommandTag{}, fmt.Errorf("unexpected SQL: %s", sql)
		}
		if !strings.Contains(sql, "status <> 'running'") {
			return pgconn.CommandTag{}, fmt.Errorf("purge must leave running jobs: %s", sql)
		}
		require.Len(t, args, 2)
		require.Equal(t, testID(7), args[0])
		require.Equal(t, purgeBatchSize, args[1])
		if len(db.ExecCalls) == 1 {
			return fakedb.Deleted(int64(purgeBatchSize)), nil
		}
		return fakedb.Deleted(2), nil
	}

	require.NoError(t, NewTenantPurger(db).PurgeTenant(context.Background(), testID(7)))
	assert.Len(t, db.ExecCalls, 2, "a full page is followed by another delete")
}

func TestPurgeTenantRejectsInvalidTenant(t *testing.T) {
	db := &fakedb.DB{}
	err := PurgeTenant(context.Background(), db, uuid.Nil)
	require.ErrorIs(t, err, ErrInvalidTenant)
	assert.Empty(t, db.ExecCalls, "an invalid tenant must not reach the database")
}

func TestPurgeTenantRequiresDatabase(t *testing.T) {
	err := PurgeTenant(context.Background(), nil, testID(7))
	require.True(t, errors.Is(err, ErrNoPool), "err = %v", err)
}

func TestPurgeTenantStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := &fakedb.DB{}
	db.ExecFn = func(context.Context, string, ...any) (pgconn.CommandTag, error) {
		return fakedb.Deleted(int64(purgeBatchSize)), nil
	}
	err := PurgeTenant(ctx, db, testID(7))
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, db.ExecCalls)
}
