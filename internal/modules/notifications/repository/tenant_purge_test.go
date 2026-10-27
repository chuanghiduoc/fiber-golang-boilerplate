package repository_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	notificationsrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/fakedb"
)

func TestTenantPurgerDrainsNotificationsInBoundedBatches(t *testing.T) {
	db := &fakedb.DB{}
	calls := map[string]int{}
	db.ExecFn = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		table := ""
		switch {
		case strings.Contains(sql, "DELETE FROM notification_preferences"):
			table = "preferences"
		case strings.Contains(sql, "DELETE FROM notifications"):
			table = "notifications"
		}
		require.NotEmpty(t, table, "unexpected SQL: %s", sql)
		require.Len(t, args, 2)
		require.Equal(t, testID(7), args[0])
		require.Equal(t, tenancycontracts.PurgeBatchSize, args[1])
		calls[table]++
		if calls[table] == 1 {
			return fakedb.Deleted(int64(tenancycontracts.PurgeBatchSize)), nil
		}
		return fakedb.Deleted(0), nil
	}

	require.NoError(t, notificationsrepo.NewTenantPurger(db).PurgeTenant(context.Background(), testID(7)))
	assert.Equal(t, 2, calls["preferences"])
	assert.Equal(t, 2, calls["notifications"])
}
