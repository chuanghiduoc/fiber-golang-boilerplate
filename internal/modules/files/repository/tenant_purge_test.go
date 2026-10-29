package repository_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

func TestTenantPurgerDeletesFileBlobsThenRowsInBoundedBatches(t *testing.T) {
	tenantID := testID(7)
	db := &purgeDB{}
	queryCalls := 0
	db.queryFn = func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
		require.Len(t, args, 2)
		require.Equal(t, tenantID, args[0])
		require.Equal(t, tenancycontracts.PurgeBatchSize, args[1])
		queryCalls++
		if queryCalls > 1 {
			return &purgeRows{}, nil
		}
		rows := make([][]any, tenancycontracts.PurgeBatchSize)
		for i := range rows {
			rows[i] = []any{testID(i + 1), fmt.Sprintf("%s/1/%d.bin", tenantID, i+1)}
		}
		return &purgeRows{rows: rows}, nil
	}
	db.execFn = func(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
		require.Len(t, args, 2)
		require.Equal(t, tenantID, args[0])
		ids, ok := args[1].([]uuid.UUID)
		require.True(t, ok)
		require.Len(t, ids, int(tenancycontracts.PurgeBatchSize))
		return pgconn.NewCommandTag(fmt.Sprintf("DELETE %d", len(ids))), nil
	}
	store := &purgeBlobStore{}

	require.NoError(t, filesrepo.NewTenantPurger(db, store).PurgeTenant(context.Background(), tenantID))

	assert.Len(t, store.deleted, int(tenancycontracts.PurgeBatchSize))
	assert.Equal(t, 2, queryCalls, "the drained page is followed by an empty select")
	assert.Equal(t, 1, db.execCalls)
}

func TestTenantPurgerDoesNotDeleteRowsWhenABlobDeleteFails(t *testing.T) {
	tenantID := testID(7)
	db := &purgeDB{}
	db.queryFn = func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return &purgeRows{rows: [][]any{{testID(1), fmt.Sprintf("%s/1/1.bin", tenantID)}}}, nil
	}
	db.execFn = func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
		t.Fatal("row delete must not run after a blob failure")
		return pgconn.CommandTag{}, nil
	}
	store := &purgeBlobStore{err: errors.New("object store unavailable")}

	err := filesrepo.NewTenantPurger(db, store).PurgeTenant(context.Background(), tenantID)
	require.Error(t, err)
	assert.Zero(t, db.execCalls)
}

func TestTenantPurgerBoundsConcurrentBlobDeletes(t *testing.T) {
	tenantID := testID(7)
	db := &purgeDB{}
	queryCalls := 0
	db.queryFn = func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		queryCalls++
		if queryCalls > 1 {
			return &purgeRows{}, nil
		}
		rows := make([][]any, tenancycontracts.PurgeBatchSize)
		for i := range rows {
			rows[i] = []any{testID(i + 1), fmt.Sprintf("%s/1/%d.bin", tenantID, i+1)}
		}
		return &purgeRows{rows: rows}, nil
	}
	db.execFn = func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
		return pgconn.NewCommandTag("DELETE 500"), nil
	}
	store := &blockedBlobStore{
		started: make(chan string, int(tenancycontracts.PurgeBatchSize)),
		release: make(chan struct{}),
	}

	done := make(chan error, 1)
	go func() {
		done <- filesrepo.NewTenantPurger(db, store).PurgeTenant(context.Background(), tenantID)
	}()

	for i := 0; i < int(tenantPurgeTestConcurrency); i++ {
		select {
		case <-store.started:
		case <-time.After(2 * time.Second):
			t.Fatal("blob deletes did not start")
		}
	}
	select {
	case <-store.started:
		t.Fatal("more than the configured number of blob deletes started concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	close(store.release)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("purge did not finish after releasing blob deletes")
	}
}

// purgeDB is the minimal filesqlc.DBTX double this test needs; the shared
// fakedb scanner only knows int64/string.
type purgeDB struct {
	queryFn   func(context.Context, string, ...any) (pgx.Rows, error)
	execFn    func(context.Context, string, ...any) (pgconn.CommandTag, error)
	execCalls int
}

func (d *purgeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.execCalls++
	return d.execFn(ctx, sql, args...)
}

func (d *purgeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return d.queryFn(ctx, sql, args...)
}

func (d *purgeDB) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("purgeDB: QueryRow is not used by the tenant purger")
}

// purgeRows serves the two columns the purge select scans: id and
// storage_path. It embeds pgx.Rows so unused methods fail loudly.
type purgeRows struct {
	pgx.Rows
	rows [][]any
	pos  int
}

func (r *purgeRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *purgeRows) Scan(dest ...any) error {
	values := r.rows[r.pos-1]
	if len(dest) != len(values) {
		return fmt.Errorf("purgeRows: scan %d destinations into %d values", len(dest), len(values))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *uuid.UUID:
			v, ok := values[i].(uuid.UUID)
			if !ok {
				return fmt.Errorf("purgeRows: cannot scan %T into *uuid.UUID", values[i])
			}
			*d = v
		case *string:
			v, ok := values[i].(string)
			if !ok {
				return fmt.Errorf("purgeRows: cannot scan %T into *string", values[i])
			}
			*d = v
		default:
			return fmt.Errorf("purgeRows: unsupported scan destination %T", dest[i])
		}
	}
	return nil
}

func (r *purgeRows) Err() error { return nil }

func (r *purgeRows) Close() {}

type purgeBlobStore struct {
	storage.Storage // unused methods fail loudly if called
	err             error
	mu              sync.Mutex
	deleted         []string
}

func (s *purgeBlobStore) Delete(_ context.Context, path string) error {
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, path)
	return nil
}

const tenantPurgeTestConcurrency = 4

type blockedBlobStore struct {
	storage.Storage
	started chan string
	release chan struct{}
}

func (s *blockedBlobStore) Delete(ctx context.Context, path string) error {
	select {
	case s.started <- path:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
