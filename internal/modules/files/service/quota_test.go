package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage/scan"
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
		t.Fatalf("stored blobs = %d, want the rejected upload never stored", len(store.files))
	}
	if len(store.deletedPaths) != 0 {
		t.Fatalf("deleted blobs = %d, want no blob written at all", len(store.deletedPaths))
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
	if len(store.files) != 0 || len(store.deletedPaths) != 0 {
		t.Fatal("the over-quota upload must be rejected before Put")
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

// TestUpload_QuotaPrecheckSkipsPutAndScan proves a clearly-over-quota upload is
// rejected before the blob Put and the clamd scan, so it costs no storage I/O
// or scanner capacity. The locked check inside the transaction still runs for
// concurrent uploads that each fit alone.
func TestUpload_QuotaPrecheckSkipsPutAndScan(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	repo.files[1] = &File{ID: 1, TenantID: 1, UserID: 10, Size: 100, CreatedAt: time.Now()}
	scanner := &fakeScanner{verdict: scan.VerdictClean}
	svc := NewUploadService(repo, newFakeTx(repo), store, UploadOptions{
		TenantQuotaBytes: 100,
		Scanner:          scanner,
	})

	_, err := svc.Upload(systemCtx(), 10, "big.bin", strings.NewReader("0123456789"), 10, "application/octet-stream")
	requireQuotaExceeded(t, err)
	if len(store.files) != 0 {
		t.Fatal("an over-quota upload must not write a blob")
	}
	if scanner.calls != 0 {
		t.Fatalf("scanner calls = %d, want 0 before the quota pre-check", scanner.calls)
	}
	if repo.quotaLocks != 0 {
		t.Fatalf("quota locks = %d, want the cheap pre-check to take no lock", repo.quotaLocks)
	}
}
