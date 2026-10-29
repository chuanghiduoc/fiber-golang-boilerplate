//go:build integration

package repository_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

func countFiles(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE tenant_id = $1`, tenantID).Scan(&count))
	return count
}

func countBlobs(t *testing.T, root string) int {
	t.Helper()
	blobs := 0
	require.NoError(t, filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			blobs++
		}
		return nil
	}))
	return blobs
}

// slowUsageTx widens the lock-to-insert window inside the metadata transaction.
// With the counter lock, the second upload is still blocked before the check,
// so only one passes; without it both read the same free space and commit.
type slowUsageTx struct{ inner filesvc.Transactor }

func (s slowUsageTx) InTx(ctx context.Context, fn func(context.Context, filesvc.TxRepos) error) error {
	return s.inner.InTx(ctx, func(ctx context.Context, tx filesvc.TxRepos) error {
		tx.Files = &slowUsageRepo{FileRepository: tx.Files}
		return fn(ctx, tx)
	})
}

type slowUsageRepo struct{ filesvc.FileRepository }

func (r *slowUsageRepo) LockStorageUsage(ctx context.Context, userID uuid.UUID, scope filesvc.QuotaScope) (filesvc.QuotaUsage, error) {
	usage, err := r.FileRepository.LockStorageUsage(ctx, userID, scope)
	time.Sleep(100 * time.Millisecond)
	return usage, err
}

// TestConcurrentUploadsCannotBothPassQuota is the regression for a check-then-
// insert quota: without the per-tenant advisory lock, both uploads read zero
// usage and both commit, exceeding the quota 2x.
func TestConcurrentUploadsCannotBothPassQuota(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass

	memberID := createFilesMember(t, pool, tenant.DefaultID, "quota-race@example.com")
	root := t.TempDir()
	store, err := storage.NewLocalStorage(root)
	require.NoError(t, err)

	const quota = 100
	svc := filesvc.NewUploadService(
		filesrepo.NewFileRepository(fx.Scoped()),
		slowUsageTx{inner: filesrepo.NewTransactor(fx.Tx(), nil, nil)},
		store,
		authz.NewAuthorizer(nil),
		filesvc.UploadOptions{TenantQuotaBytes: quota},
	)
	reqCtx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))

	type result struct {
		err      error
		metadata *filesvc.File
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			file, uploadErr := svc.Upload(reqCtx, memberID, "race.bin", strings.NewReader(strings.Repeat("x", 60)), 60, "application/octet-stream")
			results <- result{err: uploadErr, metadata: file}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var succeeded, rejected int
	for res := range results {
		if res.err == nil {
			succeeded++
			continue
		}
		var appErr *apperror.AppError
		require.True(t, errors.As(res.err, &appErr), "unexpected error: %v", res.err)
		assert.Equal(t, string(catalog.CodeFileQuotaExceeded), appErr.Code)
		rejected++
	}
	assert.Equal(t, 1, succeeded, "exactly one concurrent upload may fit the quota")
	assert.Equal(t, 1, rejected)
	assert.Equal(t, 1, countFiles(t, pool, tenant.DefaultID))
	assert.Equal(t, 1, countBlobs(t, root), "the rejected upload's blob must be removed")
}

func storedUsage(t *testing.T, pool *pgxpool.Pool, tenantID, userID uuid.UUID) (tenantBytes, userBytes int64) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT
			COALESCE((SELECT SUM(bytes) FROM files_storage_usage
				WHERE tenant_id = $1), 0),
			COALESCE((SELECT bytes FROM files_storage_usage
				WHERE tenant_id = $1 AND user_id = $2), 0)`,
		tenantID, userID).Scan(&tenantBytes, &userBytes))
	return tenantBytes, userBytes
}

func countTenantAggregateUsageRows(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM files_storage_usage
		WHERE tenant_id = $1 AND user_id IS NULL`, tenantID).Scan(&count))
	return count
}

// TestStorageUsageCounterLifecycle proves counters are per user, tenant usage is
// their SUM, and soft-delete/restore keep the blob's bytes counted while a hard
// delete releases them.
func TestStorageUsageCounterLifecycle(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	memberID := createFilesMember(t, pool, tenant.DefaultID, "quota-counter@example.com")
	otherMemberID := createFilesMember(t, pool, tenant.DefaultID, "quota-counter-other@example.com")

	var fileID uuid.UUID
	require.NoError(t, pool.QueryRow(context.Background(), `
		INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		VALUES ($1, $2, 'counter.bin', $3, 'application/octet-stream', 17)
		RETURNING id`,
		tenant.DefaultID, memberID, tenant.DefaultID.String()+"/"+memberID.String()+"/counter.bin",
	).Scan(&fileID))

	tenantBytes, userBytes := storedUsage(t, pool, tenant.DefaultID, memberID)
	assert.Equal(t, int64(17), tenantBytes)
	assert.Equal(t, int64(17), userBytes)
	assert.Zero(t, countTenantAggregateUsageRows(t, pool, tenant.DefaultID),
		"the trigger must not write a tenant aggregate row")

	_, err := pool.Exec(context.Background(), `
		INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		VALUES ($1, $2, 'other.bin', $3, 'application/octet-stream', 5)`,
		tenant.DefaultID, otherMemberID, tenant.DefaultID.String()+"/"+otherMemberID.String()+"/other.bin")
	require.NoError(t, err)
	tenantBytes, userBytes = storedUsage(t, pool, tenant.DefaultID, memberID)
	assert.Equal(t, int64(22), tenantBytes, "tenant usage is the sum of user counters")
	assert.Equal(t, int64(17), userBytes)

	_, err = pool.Exec(context.Background(), `UPDATE files SET deleted_at = now() WHERE id = $1`, fileID)
	require.NoError(t, err)
	tenantBytes, userBytes = storedUsage(t, pool, tenant.DefaultID, memberID)
	assert.Equal(t, int64(22), tenantBytes, "soft-deleted blobs still occupy storage")
	assert.Equal(t, int64(17), userBytes, "soft-deleted blobs still occupy storage")

	_, err = pool.Exec(context.Background(), `UPDATE files SET deleted_at = NULL WHERE id = $1`, fileID)
	require.NoError(t, err)
	tenantBytes, userBytes = storedUsage(t, pool, tenant.DefaultID, memberID)
	assert.Equal(t, int64(22), tenantBytes)
	assert.Equal(t, int64(17), userBytes)

	_, err = pool.Exec(context.Background(), `DELETE FROM files WHERE id = $1`, fileID)
	require.NoError(t, err)
	tenantBytes, userBytes = storedUsage(t, pool, tenant.DefaultID, memberID)
	assert.Equal(t, int64(5), tenantBytes, "the other user's bytes remain")
	assert.Zero(t, userBytes)
}

// TestStorageUsageTenantScopeSumsUserCounters exercises the repository query
// used by the tenant quota: it must see both users, while a user-only read sees
// only one row and never adds tenant-wide usage.
func TestStorageUsageTenantScopeSumsUserCounters(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	userA := createFilesMember(t, pool, tenant.DefaultID, "quota-sum-a@example.com")
	userB := createFilesMember(t, pool, tenant.DefaultID, "quota-sum-b@example.com")

	for _, file := range []struct {
		userID uuid.UUID
		name   string
		size   int64
	}{
		{userA, "a.bin", 7},
		{userB, "b.bin", 11},
	} {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
			VALUES ($1, $2, $3, $4, 'application/octet-stream', $5)`,
			tenant.DefaultID, file.userID, file.name,
			tenant.DefaultID.String()+"/"+file.userID.String()+"/"+file.name, file.size)
		require.NoError(t, err)
	}

	repo := filesrepo.NewFileRepository(fx.Scoped())
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)
	tenantUsage, err := repo.StorageUsage(ctx, userA, filesvc.QuotaScope{Tenant: true})
	require.NoError(t, err)
	assert.Equal(t, int64(18), tenantUsage.TenantBytes)
	assert.Zero(t, tenantUsage.UserBytes)

	userUsage, err := repo.StorageUsage(ctx, userA, filesvc.QuotaScope{User: true})
	require.NoError(t, err)
	assert.Zero(t, userUsage.TenantBytes)
	assert.Equal(t, int64(7), userUsage.UserBytes)
}

// quotaLockBarrier reports when an upload's authoritative quota lock has been
// acquired and holds the transaction until both uploads have reached it.
type quotaLockBarrier struct {
	filesvc.FileRepository
	acquired chan<- uuid.UUID
	release  <-chan struct{}
}

func (r *quotaLockBarrier) LockStorageUsage(ctx context.Context, userID uuid.UUID, scope filesvc.QuotaScope) (filesvc.QuotaUsage, error) {
	usage, err := r.FileRepository.LockStorageUsage(ctx, userID, scope)
	if err != nil {
		return filesvc.QuotaUsage{}, err
	}
	select {
	case r.acquired <- userID:
	case <-ctx.Done():
		return filesvc.QuotaUsage{}, ctx.Err()
	}
	select {
	case <-r.release:
		return usage, nil
	case <-ctx.Done():
		return filesvc.QuotaUsage{}, ctx.Err()
	}
}

type quotaBarrierTx struct {
	inner    filesvc.Transactor
	acquired chan uuid.UUID
	release  chan struct{}
}

func (t quotaBarrierTx) InTx(ctx context.Context, fn func(context.Context, filesvc.TxRepos) error) error {
	return t.inner.InTx(ctx, func(ctx context.Context, repos filesvc.TxRepos) error {
		repos.Files = &quotaLockBarrier{
			FileRepository: repos.Files,
			acquired:       t.acquired,
			release:        t.release,
		}
		return fn(ctx, repos)
	})
}

// TestUserQuotaUploadsForDifferentUsersDoNotShareALock proves the upload path
// locks only the uploading user's counter when the tenant quota is off. If the
// two uploads shared a tenant row, the second could not report acquisition
// while the first is held here.
func TestUserQuotaUploadsForDifferentUsersDoNotShareALock(t *testing.T) {
	fx := setupFilesDB(t)
	pool := fx.Bypass
	userA := createFilesMember(t, pool, tenant.DefaultID, "quota-lock-a@example.com")
	userB := createFilesMember(t, pool, tenant.DefaultID, "quota-lock-b@example.com")

	root := t.TempDir()
	store, err := storage.NewLocalStorage(root)
	require.NoError(t, err)

	acquired := make(chan uuid.UUID, 2)
	release := make(chan struct{})
	tx := quotaBarrierTx{
		inner:    filesrepo.NewTransactor(fx.Tx(), nil, nil),
		acquired: acquired,
		release:  release,
	}
	svc := filesvc.NewUploadService(
		filesrepo.NewFileRepository(fx.Scoped()),
		tx,
		store,
		authz.NewAuthorizer(nil),
		filesvc.UploadOptions{UserQuotaBytes: 1000},
	)
	reqCtx := authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))

	results := make(chan error, 2)
	for _, userID := range []uuid.UUID{userA, userB} {
		go func(userID uuid.UUID) {
			_, uploadErr := svc.Upload(reqCtx, userID, "race.bin", strings.NewReader(strings.Repeat("x", 10)), 10, "application/octet-stream")
			results <- uploadErr
		}(userID)
	}

	seen := make(map[uuid.UUID]bool, 2)
	timeout := time.After(3 * time.Second)
	for len(seen) < 2 {
		select {
		case userID := <-acquired:
			seen[userID] = true
		case <-timeout:
			close(release)
			t.Fatal("the two user-quota uploads did not acquire independent counter locks concurrently")
		}
	}
	close(release)

	for i := 0; i < 2; i++ {
		require.NoError(t, <-results)
	}
	assert.Equal(t, 2, countFiles(t, pool, tenant.DefaultID))
	assert.Equal(t, 2, countBlobs(t, root))
	assert.Len(t, seen, 2)
}
