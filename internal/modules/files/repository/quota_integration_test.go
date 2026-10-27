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

// slowUsageTx widens the read-to-insert window inside the metadata
// transaction. With the per-tenant lock, the second upload is still blocked
// before StorageUsage, so only one passes; without the lock both read the same
// free space during the pause and both commit.
type slowUsageTx struct{ inner filesvc.Transactor }

func (s slowUsageTx) InTx(ctx context.Context, fn func(context.Context, filesvc.TxRepos) error) error {
	return s.inner.InTx(ctx, func(ctx context.Context, tx filesvc.TxRepos) error {
		tx.Files = &slowUsageRepo{FileRepository: tx.Files}
		return fn(ctx, tx)
	})
}

type slowUsageRepo struct{ filesvc.FileRepository }

func (r *slowUsageRepo) StorageUsage(ctx context.Context, userID uuid.UUID) (tenantBytes, userBytes int64, err error) {
	tenantBytes, userBytes, err = r.FileRepository.StorageUsage(ctx, userID)
	time.Sleep(100 * time.Millisecond)
	return tenantBytes, userBytes, err
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
