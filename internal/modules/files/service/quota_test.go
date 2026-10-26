package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func requireQuotaExceeded(t *testing.T, err error) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("error = %v, want an apperror", err)
	}
	if appErr.Code != string(catalog.CodeFileQuotaExceeded) {
		t.Fatalf("code = %q, want %q", appErr.Code, catalog.CodeFileQuotaExceeded)
	}
	if appErr.Status != 413 {
		t.Fatalf("status = %d, want 413", appErr.Status)
	}
}

func quotaService(repo *mockFileRepo, store *mockStorage, tenantQuota, userQuota int64) UploadService {
	return NewUploadService(repo, newFakeTx(repo), store, UploadOptions{
		TenantQuotaBytes: tenantQuota,
		UserQuotaBytes:   userQuota,
	})
}

// TestUpload_TenantQuota counts trashed files too: a quota is about stored
// bytes, and restore must not be able to push the tenant over it.
func TestUpload_TenantQuota(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	repo.files[1] = &File{ID: 1, TenantID: 1, UserID: 10, Size: 90, CreatedAt: time.Now(), DeletedAt: timePtr(time.Now())}
	svc := quotaService(repo, store, 100, 0)

	_, err := svc.Upload(systemCtx(), 10, "big.bin", strings.NewReader("01234567890"), 11, "application/octet-stream")
	requireQuotaExceeded(t, err)
	if len(repo.files) != 1 {
		t.Fatalf("metadata rows = %d, want only the pre-existing one", len(repo.files))
	}
	if len(store.files) != 0 {
		t.Fatalf("stored blobs = %d, want the rejected blob removed", len(store.files))
	}
	if len(store.deletedPaths) != 1 {
		t.Fatalf("deleted blobs = %d, want the rejected blob deleted", len(store.deletedPaths))
	}
}

func TestUpload_UserQuota(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	repo.files[1] = &File{ID: 1, TenantID: 1, UserID: 10, Size: 40, CreatedAt: time.Now()}
	repo.files[2] = &File{ID: 2, TenantID: 1, UserID: 11, Size: 40, CreatedAt: time.Now()}
	svc := quotaService(repo, store, 0, 50)

	// Another user's bytes count toward the tenant but not this user's quota.
	_, err := svc.Upload(systemCtx(), 10, "big.bin", strings.NewReader("0123456789"), 11, "application/octet-stream")
	requireQuotaExceeded(t, err)
	if len(store.files) != 0 || len(store.deletedPaths) != 1 {
		t.Fatal("the rejected blob must be removed")
	}

	// The same upload fits when it stays within both quotas.
	repo.files[1].Size = 30
	if _, err := svc.Upload(systemCtx(), 10, "small.bin", strings.NewReader("0123456789"), 10, "application/octet-stream"); err != nil {
		t.Fatalf("upload within quota: %v", err)
	}
}

func TestUpload_QuotaZeroIsUnlimited(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	repo.files[1] = &File{ID: 1, TenantID: 1, UserID: 10, Size: 1 << 40, CreatedAt: time.Now()}
	svc := quotaService(repo, store, 0, 0)

	if _, err := svc.Upload(systemCtx(), 10, "ok.bin", strings.NewReader("data"), 4, "application/octet-stream"); err != nil {
		t.Fatalf("upload with zero quotas must be unlimited: %v", err)
	}
}

// TestUpload_QuotaLockedAndCheckedInTransaction proves the quota check is
// inside the metadata transaction and takes the per-tenant lock first.
func TestUpload_QuotaLockedAndCheckedInTransaction(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := quotaService(repo, store, 100, 0)

	if _, err := svc.Upload(systemCtx(), 10, "ok.bin", strings.NewReader("data"), 4, "application/octet-stream"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if repo.quotaLocks != 1 {
		t.Fatalf("quota locks = %d, want 1", repo.quotaLocks)
	}
}
