package repository_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	apikeyrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/fakedb"
)

func testID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0192c9b0-7e4a-7000-8000-%012d", n))
}

func TestTenantPurgerDeletesAPIKeysInBoundedBatches(t *testing.T) {
	tenantID := testID(7)
	db := &fakedb.DB{}
	db.ExecFn = func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		if !strings.Contains(sql, "DELETE FROM api_keys") {
			return pgconn.CommandTag{}, fmt.Errorf("unexpected SQL: %s", sql)
		}
		require.Len(t, args, 2)
		require.Equal(t, tenantID, args[0])
		require.Equal(t, tenancycontracts.PurgeBatchSize, args[1])
		if len(db.ExecCalls) == 1 {
			return fakedb.Deleted(int64(tenancycontracts.PurgeBatchSize)), nil
		}
		return fakedb.Deleted(0), nil
	}

	require.NoError(t, apikeyrepo.NewTenantPurger(db).PurgeTenant(context.Background(), tenantID))
	assert.Len(t, db.ExecCalls, 2, "a full page is followed by another select")
}
