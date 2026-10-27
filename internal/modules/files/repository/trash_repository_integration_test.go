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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const trashRetentionDays = 30

// insertFile creates a metadata row (soft-deleted deletedAgo ago when
// deletedAgo > 0) and, when withBlob, its blob.
func insertFile(t *testing.T, pool *pgxpool.Pool, store storage.Storage, tenantID, userID uuid.UUID, name string, deletedAgo time.Duration, withBlob bool) (id uuid.UUID, path string) {
	t.Helper()
	ctx := context.Background()
	path = fmt.Sprintf("%s/%s/%s.png", tenantID, userID, name)
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

// failingBlobStore fails Delete for one path and delegates everything else.
type failingBlobStore struct {
	storage.Storage
	failPath string
	fails    int
}

func (s *failingBlobStore) Delete(ctx context.Context, path string) error {
	if path == s.failPath {
		s.fails++
		return errors.New("s3 unavailable")
	}
	return s.Storage.Delete(ctx, path)
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

func fileIDs(t *testing.T, pool *pgxpool.Pool) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id FROM files ORDER BY id`)
	require.NoError(t, err)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	require.NoError(t, err)
	return ids
}

func TestTrashPurgeRemovesExpiredTrashAcrossTenants(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	root := t.TempDir()
	store, err := storage.NewLocalStorage(root)
	require.NoError(t, err)

	tenantB := createFilesTenant(t, pool, "trash-b")
	userA := createFilesMember(t, pool, tenant.DefaultID, "trash-a@example.com")
	userB := createFilesMember(t, pool, tenantB, "trash-b@example.com")

	const old, recent = 40 * 24 * time.Hour, 5 * 24 * time.Hour
	_, oldA := insertFile(t, pool, store, tenant.DefaultID, userA, "old-a", old, true)
	_, oldB := insertFile(t, pool, store, tenantB, userB, "old-b", old, true)
	insertFile(t, pool, store, tenant.DefaultID, userA, "old-missing-blob", old, false)
	recentID, recentPath := insertFile(t, pool, store, tenant.DefaultID, userA, "recent", recent, true)
	liveID, livePath := insertFile(t, pool, store, tenantB, userB, "live", 0, true)

	purger := filesvc.NewTrashPurger(filesrepo.NewTrashRepository(fx.Tx()), store, trashRetentionDays)
	ctx := context.Background()
	require.NoError(t, purger.HandleJob(ctx, nil))

	assert.Equal(t, []uuid.UUID{recentID, liveID}, fileIDs(t, pool),
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
	assert.Equal(t, []uuid.UUID{recentID, liveID}, fileIDs(t, pool))
}

// TestTrashPurgeClaimBlocksRestore proves a restore can never revive a row
// whose blob the purge already deleted: the claim commits before purge runs,
// so restore fails while the blob is being deleted and still fails after the
// purge errors out (blob gone, row kept). The next run finishes the row.
func TestTrashPurgeClaimBlocksRestore(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	root := t.TempDir()
	store, err := storage.NewLocalStorage(root)
	require.NoError(t, err)
	userA := createFilesMember(t, pool, tenant.DefaultID, "trash-claim@example.com")
	id, path := insertFile(t, pool, store, tenant.DefaultID, userA, "claimed", 40*24*time.Hour, true)

	ctx := context.Background()
	tenantCtx := tenant.WithID(ctx, tenant.DefaultID)
	files := filesrepo.NewFileRepository(fx.Scoped())
	repo := filesrepo.NewTrashRepository(fx.Tx())
	failure := errors.New("hard delete never ran")
	_, err = repo.PurgeBatch(ctx, trashRetentionDays, 10, func(ctx context.Context, batch []filesvc.TrashedFile) ([]uuid.UUID, []uuid.UUID, error) {
		require.Len(t, batch, 1)
		_, restoreErr := files.Restore(tenantCtx, id)
		assert.ErrorIs(t, restoreErr, apperror.ErrNotFound, "a claimed row is not restorable")
		require.NoError(t, store.Delete(ctx, path))
		return nil, nil, failure
	})
	require.ErrorIs(t, err, failure)

	assert.Equal(t, []uuid.UUID{id}, fileIDs(t, pool), "the failed batch keeps its row")
	assert.False(t, blobExists(t, root, path))
	_, err = files.Restore(tenantCtx, id)
	assert.ErrorIs(t, err, apperror.ErrNotFound, "the claim survives the failed purge")

	deleted, err := repo.PurgeBatch(ctx, trashRetentionDays, 10, func(_ context.Context, batch []filesvc.TrashedFile) ([]uuid.UUID, []uuid.UUID, error) {
		return []uuid.UUID{batch[0].ID}, nil, nil
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted, "the next run reclaims and finishes the row")
	assert.Empty(t, fileIDs(t, pool))
}

// TestTrashPurgeFailedBlobDoesNotBlockNewerRows proves a permanently failing
// blob is deferred on its own row instead of stalling every newer row: the
// claim is kept (the row is never restorable again), the attempt is counted,
// and the next run continues with rows outside the backoff.
func TestTrashPurgeFailedBlobDoesNotBlockNewerRows(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	root := t.TempDir()
	base, err := storage.NewLocalStorage(root)
	require.NoError(t, err)
	store := &failingBlobStore{Storage: base}

	userA := createFilesMember(t, pool, tenant.DefaultID, "trash-retry@example.com")
	const old = 40 * 24 * time.Hour
	stuckID, stuckPath := insertFile(t, pool, store, tenant.DefaultID, userA, "stuck", old, true)
	insertFile(t, pool, store, tenant.DefaultID, userA, "ok", old, true)
	store.failPath = stuckPath

	ctx := context.Background()
	purger := filesvc.NewTrashPurger(filesrepo.NewTrashRepository(fx.Tx()), store, trashRetentionDays)
	require.NoError(t, purger.HandleJob(ctx, nil), "a blob failure is recorded, not returned as a job error")
	assert.Equal(t, []uuid.UUID{stuckID}, fileIDs(t, pool), "the rest of the batch is purged")
	assert.True(t, blobExists(t, root, stuckPath), "the failed delete leaves the blob for the retry")

	var attempts int
	var retryAfter *time.Time
	var claimedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT purge_attempts, purge_retry_after, purge_started_at FROM files WHERE id = $1`, stuckID,
	).Scan(&attempts, &retryAfter, &claimedAt))
	assert.Equal(t, 1, attempts)
	require.NotNil(t, retryAfter)
	assert.True(t, retryAfter.After(time.Now()), "the failed row is deferred into the future")
	require.NotNil(t, claimedAt, "the first delete attempt makes the row permanently non-restorable")

	// A newer expired row is purged on the next run while the failed row waits.
	_, newerPath := insertFile(t, pool, store, tenant.DefaultID, userA, "newer", old, true)
	require.NoError(t, purger.HandleJob(ctx, nil))
	assert.Equal(t, []uuid.UUID{stuckID}, fileIDs(t, pool), "the backoff row does not block newer rows")
	assert.False(t, blobExists(t, root, newerPath))
	assert.Equal(t, 1, store.fails, "the row in backoff is skipped, not retried immediately")

	// Keeping the claim means the user can never restore a row whose blob
	// delete was already attempted.
	_, err = filesrepo.NewFileRepository(fx.Scoped()).Restore(tenant.WithID(ctx, tenant.DefaultID), stuckID)
	assert.ErrorIs(t, err, apperror.ErrNotFound, "a failed purge stays non-restorable")
}

// TestTrashPurgeSkipsRowsLockedByRestore proves the claim does not wait on a
// row a restore is still updating: it skips it, and the restore wins.
func TestTrashPurgeSkipsRowsLockedByRestore(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	userA := createFilesMember(t, pool, tenant.DefaultID, "trash-skip@example.com")
	id, _ := insertFile(t, pool, store, tenant.DefaultID, userA, "restoring", 40*24*time.Hour, true)

	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `UPDATE files SET deleted_at = NULL WHERE id = $1`, id)
	require.NoError(t, err)

	repo := filesrepo.NewTrashRepository(fx.Tx())
	deleted, err := repo.PurgeBatch(ctx, trashRetentionDays, 10, func(context.Context, []filesvc.TrashedFile) ([]uuid.UUID, []uuid.UUID, error) {
		t.Fatal("a row locked by a restore must be skipped")
		return nil, nil, nil
	})
	require.NoError(t, err)
	assert.Zero(t, deleted)
	require.NoError(t, tx.Commit(ctx))
}

// TestTrashPurgeQueryUsesPartialIndex checks the purge plan on a realistic
// table (100k live rows, 0.5% trashed) instead of an empty one.
func TestTrashPurgeQueryUsesPartialIndex(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	ctx := context.Background()
	userA := createFilesMember(t, pool, tenant.DefaultID, "trash-plan@example.com")
	_, err := pool.Exec(ctx, `
		INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size, deleted_at)
		SELECT $2, $1, 'f', 'seed/' || g, 'image/png', 1,
		       CASE WHEN g % 200 = 0 THEN now() - make_interval(days => 31 + g % 30) END
		FROM generate_series(1, 100000) AS g`, userA, tenant.DefaultID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE files`)
	require.NoError(t, err)

	// The ClaimTrashedFiles subquery (queries/trash.sql). EXPLAIN ANALYZE takes
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

// TestExistingStoragePathsScopesToTenant proves the orphan sweep lookup only
// sees the tenant in its context, so a path owned by another tenant is never
// treated as live.
func TestExistingStoragePathsScopesToTenant(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	tenantB := createFilesTenant(t, pool, "orphan-b")
	userA := createFilesMember(t, pool, tenant.DefaultID, "orphan-a@example.com")
	userB := createFilesMember(t, pool, tenantB, "orphan-b@example.com")
	_, pathA := insertFile(t, pool, store, tenant.DefaultID, userA, "live-a", 0, true)
	_, pathB := insertFile(t, pool, store, tenantB, userB, "live-b", 0, true)

	repo := filesrepo.NewFileRepository(fx.Scoped())
	orphan := fmt.Sprintf("%s/%s/orphan.png", tenant.DefaultID, testID(999))
	existing, err := repo.ExistingStoragePaths(tenant.WithID(context.Background(), tenant.DefaultID), []string{pathA, pathB, orphan})
	require.NoError(t, err)
	assert.Contains(t, existing, pathA)
	assert.NotContains(t, existing, pathB, "another tenant's row must not make a path live")
	assert.NotContains(t, existing, orphan)
}
