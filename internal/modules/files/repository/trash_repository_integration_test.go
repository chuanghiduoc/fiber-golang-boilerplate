//go:build integration

package repository_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const trashRetentionDays = 30

func setupTrashDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return pool
}

func insertTenant(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO tenants (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&id))
	return id
}

func insertUser(t *testing.T, pool *pgxpool.Pool, tenantID int64, email string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO users (tenant_id, email, name) VALUES ($1, $2, $2) RETURNING id`, tenantID, email).Scan(&id))
	return id
}

// insertFile creates a metadata row (soft-deleted deletedAgo ago when
// deletedAgo > 0) and, when withBlob, its blob.
func insertFile(t *testing.T, pool *pgxpool.Pool, store storage.Storage, tenantID, userID int64, name string, deletedAgo time.Duration, withBlob bool) (id int64, path string) {
	t.Helper()
	ctx := context.Background()
	path = fmt.Sprintf("%d/%d/%s.png", tenantID, userID, name)
	if withBlob {
		require.NoError(t, store.Put(ctx, path, bytes.NewReader([]byte(name)), int64(len(name)), "image/png"))
	}
	var deletedAt *time.Time
	if deletedAgo > 0 {
		ts := time.Now().Add(-deletedAgo)
		deletedAt = &ts
	}
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size, deleted_at)
		 VALUES ($1, $2, $3, $4, 'image/png', $5, $6) RETURNING id`,
		tenantID, userID, name, path, len(name), deletedAt).Scan(&id))
	return id, path
}

func blobExists(t *testing.T, root, path string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(path)))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	require.NoError(t, err)
	return true
}

func fileIDs(t *testing.T, pool *pgxpool.Pool) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id FROM files ORDER BY id`)
	require.NoError(t, err)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	require.NoError(t, err)
	return ids
}

func TestTrashPurgeRemovesExpiredTrashAcrossTenants(t *testing.T) {
	pool := setupTrashDB(t)
	root := t.TempDir()
	store, err := storage.NewLocalStorage(root)
	require.NoError(t, err)

	tenantB := insertTenant(t, pool, "trash-b")
	userA := insertUser(t, pool, tenant.DefaultID, "trash-a@example.com")
	userB := insertUser(t, pool, tenantB, "trash-b@example.com")

	const old, recent = 40 * 24 * time.Hour, 5 * 24 * time.Hour
	_, oldA := insertFile(t, pool, store, tenant.DefaultID, userA, "old-a", old, true)
	_, oldB := insertFile(t, pool, store, tenantB, userB, "old-b", old, true)
	insertFile(t, pool, store, tenant.DefaultID, userA, "old-missing-blob", old, false)
	recentID, recentPath := insertFile(t, pool, store, tenant.DefaultID, userA, "recent", recent, true)
	liveID, livePath := insertFile(t, pool, store, tenantB, userB, "live", 0, true)

	purger := filesvc.NewTrashPurger(filesrepo.NewTrashRepository(database.NewTxManager(pool)), store, trashRetentionDays)
	ctx := context.Background()
	require.NoError(t, purger.HandleJob(ctx, nil))

	assert.Equal(t, []int64{recentID, liveID}, fileIDs(t, pool),
		"expired trash is gone in every tenant; recent trash and live files stay")
	assert.False(t, blobExists(t, root, oldA))
	assert.False(t, blobExists(t, root, oldB))
	assert.True(t, blobExists(t, root, recentPath))
	assert.True(t, blobExists(t, root, livePath))

	var jobCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&jobCount))
	assert.Zero(t, jobCount, "the purge publishes no event, so no webhook job is enqueued")

	// At-least-once redelivery is a no-op.
	require.NoError(t, purger.HandleJob(ctx, nil))
	assert.Equal(t, []int64{recentID, liveID}, fileIDs(t, pool))
}

// TestTrashPurgeLocksRowsAgainstRestore proves a restore cannot race the
// purge: while the batch's blobs are being deleted the row is locked, and
// once the purge commits the restore finds nothing.
func TestTrashPurgeLocksRowsAgainstRestore(t *testing.T) {
	pool := setupTrashDB(t)
	root := t.TempDir()
	store, err := storage.NewLocalStorage(root)
	require.NoError(t, err)
	userA := insertUser(t, pool, tenant.DefaultID, "trash-lock@example.com")
	id, _ := insertFile(t, pool, store, tenant.DefaultID, userA, "locked", 40*24*time.Hour, true)

	ctx := context.Background()
	repo := filesrepo.NewTrashRepository(database.NewTxManager(pool))
	deleted, err := repo.PurgeBatch(ctx, trashRetentionDays, 10, func(ctx context.Context, batch []filesvc.TrashedFile) ([]int64, error) {
		require.Len(t, batch, 1)

		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`)
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `UPDATE files SET deleted_at = NULL WHERE id = $1`, id)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "55P03", pgErr.Code, "a concurrent restore must wait for the purge lock")

		return []int64{batch[0].ID}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)

	_, err = filesrepo.NewFileRepository(pool).Restore(tenant.WithID(ctx, tenant.DefaultID), id)
	assert.ErrorIs(t, err, apperror.ErrNotFound)
}

// TestTrashPurgeQueryUsesPartialIndex checks the purge plan on a realistic
// table (100k live rows, 0.5% trashed) instead of an empty one.
func TestTrashPurgeQueryUsesPartialIndex(t *testing.T) {
	pool := setupTrashDB(t)
	ctx := context.Background()
	userA := insertUser(t, pool, tenant.DefaultID, "trash-plan@example.com")
	_, err := pool.Exec(ctx, `
		INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size, deleted_at)
		SELECT 1, $1, 'f', 'seed/' || g, 'image/png', 1,
		       CASE WHEN g % 200 = 0 THEN now() - make_interval(days => 31 + g % 30) END
		FROM generate_series(1, 100000) AS g`, userA)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE files`)
	require.NoError(t, err)

	// The LockTrashedFiles query (queries/trash.sql). EXPLAIN ANALYZE takes
	// the row locks, so it runs in a transaction that rolls back.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS)
		SELECT id, tenant_id, storage_path FROM files
		WHERE deleted_at IS NOT NULL
		  AND deleted_at < now() - make_interval(days => 30)
		ORDER BY deleted_at
		LIMIT 100
		FOR UPDATE SKIP LOCKED`)
	require.NoError(t, err)
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	plan := strings.Join(lines, "\n")
	t.Log("\n" + plan)
	assert.Contains(t, plan, "idx_files_deleted_at")
}
