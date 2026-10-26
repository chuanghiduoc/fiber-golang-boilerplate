package repository_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/fakedb"
)

func TestTenantPurgerDrainsEveryWebhookTableInBoundedBatches(t *testing.T) {
	db := &fakedb.DB{}
	calls := map[string]int{}
	db.ExecFn = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		table := ""
		switch {
		case strings.Contains(sql, "DELETE FROM webhook_deliveries"):
			table = "deliveries"
		case strings.Contains(sql, "DELETE FROM webhook_events"):
			table = "events"
		case strings.Contains(sql, "DELETE FROM webhook_endpoints"):
			table = "endpoints"
		case strings.Contains(sql, "DELETE FROM webhook_inbound_events"):
			table = "inbound events"
		case strings.Contains(sql, "DELETE FROM webhook_inbound_sources"):
			table = "inbound sources"
		}
		require.NotEmpty(t, table, "unexpected SQL: %s", sql)
		require.Len(t, args, 2)
		require.Equal(t, int64(7), args[0])
		require.Equal(t, tenancycontracts.PurgeBatchSize, args[1])
		calls[table]++
		if calls[table] == 1 {
			return fakedb.Deleted(int64(tenancycontracts.PurgeBatchSize)), nil
		}
		return fakedb.Deleted(0), nil
	}

	require.NoError(t, webhookrepo.NewTenantPurger(db).PurgeTenant(context.Background(), 7))
	for _, table := range []string{"deliveries", "events", "endpoints", "inbound events", "inbound sources"} {
		assert.Equal(t, 2, calls[table], "%s must be drained to zero", table)
	}
}
