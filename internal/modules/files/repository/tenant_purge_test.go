package repository_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/fakedb"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

func TestTenantPurgerDeletesFileBlobsThenRowsInBoundedBatches(t *testing.T) {
	db := &fakedb.DB{}
	queryCalls := 0
	db.QueryFn = func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
		require.Len(t, args, 2)
		require.Equal(t, int64(7), args[0])
		require.Equal(t, tenancycontracts.PurgeBatchSize, args[1])
		queryCalls++
		if queryCalls > 1 {
			return fakedb.NewRows(), nil
		}
		rows := make([][]any, tenancycontracts.PurgeBatchSize)
		for i := range rows {
			rows[i] = []any{int64(i + 1), fmt.Sprintf("7/1/%d.bin", i+1)}
		}
		return fakedb.NewRows(rows...), nil
	}
	db.ExecFn = func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
		require.Len(t, args, 2)
		require.Equal(t, int64(7), args[0])
		ids, ok := args[1].([]int64)
		require.True(t, ok)
		require.Len(t, ids, int(tenancycontracts.PurgeBatchSize))
		return fakedb.Deleted(int64(len(ids))), nil
	}
	store := &purgeBlobStore{}

	require.NoError(t, filesrepo.NewTenantPurger(db, store).PurgeTenant(context.Background(), 7))

	assert.Len(t, store.deleted, int(tenancycontracts.PurgeBatchSize))
	assert.Len(t, db.QueryCalls, 2, "the drained page is followed by an empty select")
	assert.Len(t, db.ExecCalls, 1)
}

type purgeBlobStore struct {
	storage.Storage // unused methods fail loudly if called
	deleted         []string
}

func (s *purgeBlobStore) Delete(_ context.Context, path string) error {
	s.deleted = append(s.deleted, path)
	return nil
}
