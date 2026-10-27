package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

func newTestUploadService(repo *mockFileRepo, store *mockStorage) UploadService {
	return NewUploadService(repo, newFakeTx(repo), store, testAuthorizer(), UploadOptions{})
}

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

func TestUpload(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		resp, err := svc.Upload(systemCtx(), testID(1), "photo.jpg", strings.NewReader("image-data"), 10, "image/jpeg")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if resp.OriginalName != "photo.jpg" {
			t.Errorf("expected original name photo.jpg, got %s", resp.OriginalName)
		}
		if resp.MimeType != "image/jpeg" {
			t.Errorf("expected mime type image/jpeg, got %s", resp.MimeType)
		}
		if resp.Size != 10 {
			t.Errorf("expected size 10, got %d", resp.Size)
		}
		wantPrefix := testID(1).String() + "/" + testID(1).String() + "/"
		if !strings.HasPrefix(resp.StoragePath, wantPrefix) || !strings.HasSuffix(resp.StoragePath, ".jpg") {
			t.Errorf("storage path = %q, want %s<uuid>.jpg", resp.StoragePath, wantPrefix)
		}
		// Verify file was stored
		if len(store.files) != 1 {
			t.Errorf("expected 1 file in storage, got %d", len(store.files))
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		store.putErr = fmt.Errorf("disk full")
		svc := newTestUploadService(repo, store)

		_, err := svc.Upload(systemCtx(), testID(1), "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
		if err == nil {
			t.Fatal("expected error for storage failure")
		}
		if !strings.Contains(err.Error(), "failed to store file") {
			t.Errorf("expected storage error message, got %q", err.Error())
		}
		// No file should be in the DB either
		if len(repo.files) != 0 {
			t.Error("no file should be saved to DB when storage fails")
		}
	})

	t.Run("DB failure cleans up storage", func(t *testing.T) {
		store := newMockStorage()
		// Use a special repo that always fails on Create
		failRepo := &failingFileRepo{mockFileRepo: newMockFileRepo(), createErr: fmt.Errorf("db error")}
		svc := NewUploadService(failRepo, newFakeTx(failRepo), store, testAuthorizer(), UploadOptions{})

		_, err := svc.Upload(systemCtx(), testID(1), "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
		requireStatus(t, err, 500)
		// Storage should be cleaned up
		if len(store.files) != 0 {
			t.Error("storage should be cleaned up after DB failure")
		}
	})

	t.Run("duplicate storage path is a conflict", func(t *testing.T) {
		store := newMockStorage()
		failRepo := &failingFileRepo{mockFileRepo: newMockFileRepo(), createErr: ErrStoragePathTaken}
		svc := NewUploadService(failRepo, newFakeTx(failRepo), store, testAuthorizer(), UploadOptions{})

		_, err := svc.Upload(systemCtx(), testID(1), "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
		requireStatus(t, err, 409)
		if len(store.files) != 0 {
			t.Error("storage should be cleaned up after a conflict")
		}
	})
}

// failingFileRepo wraps mockFileRepo and fails Create with createErr.
type failingFileRepo struct {
	*mockFileRepo
	createErr error
}

func (r *failingFileRepo) Create(ctx context.Context, input NewFile) (*File, error) {
	if r.createErr != nil {
		return nil, r.createErr
	}
	return r.mockFileRepo.Create(ctx, input)
}

// ---------------------------------------------------------------------------
// GetFileInfo
// ---------------------------------------------------------------------------

func TestGetFileInfo(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		resp, err := svc.GetFileInfo(systemCtx(), testID(1), testID(10))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if resp.OriginalName != "doc.pdf" {
			t.Errorf("expected doc.pdf, got %s", resp.OriginalName)
		}
	})

	t.Run("not found", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		_, err := svc.GetFileInfo(systemCtx(), testID(999), testID(10))
		if err == nil {
			t.Fatal("expected not found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if resolvedStatus(appErr) != 404 {
			t.Errorf("expected 404, got %d", resolvedStatus(appErr))
		}
	})

	t.Run("forbidden - wrong user", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, err := svc.GetFileInfo(systemCtx(), testID(1), testID(99)) // wrong user
		if err == nil {
			t.Fatal("expected not-found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if resolvedStatus(appErr) != 404 {
			t.Errorf("expected 404, got %d", resolvedStatus(appErr))
		}
	})
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestUploadDelete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		err := svc.Delete(systemCtx(), testID(1), testID(10))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		// Soft delete: file stays in repo but has DeletedAt set
		f := repo.files[testID(1)]
		if f.DeletedAt == nil {
			t.Error("expected DeletedAt to be set (soft delete)")
		}
	})

	t.Run("not found", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		err := svc.Delete(systemCtx(), testID(999), testID(10))
		if err == nil {
			t.Fatal("expected not found error")
		}
		if !strings.Contains(err.Error(), "file not found") {
			t.Errorf("expected 'file not found', got %q", err.Error())
		}
	})

	t.Run("forbidden - wrong user", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		err := svc.Delete(systemCtx(), testID(1), testID(99)) // wrong user
		if err == nil {
			t.Fatal("expected not-found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if resolvedStatus(appErr) != 404 {
			t.Errorf("expected 404, got %d", resolvedStatus(appErr))
		}
	})
}

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

func TestUploadDownload(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}
		store.files["10/abc.pdf"] = []byte("pdf-content")

		file, reader, err := svc.Download(systemCtx(), testID(1), testID(10))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if file.OriginalName != "doc.pdf" {
			t.Errorf("expected doc.pdf, got %s", file.OriginalName)
		}
		if reader == nil {
			t.Fatal("expected non-nil reader")
		}
		_ = reader.Close()
	})

	t.Run("not found", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		_, _, err := svc.Download(systemCtx(), testID(999), testID(10))
		if err == nil {
			t.Fatal("expected not found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if resolvedStatus(appErr) != 404 {
			t.Errorf("expected 404, got %d", resolvedStatus(appErr))
		}
	})

	t.Run("forbidden - wrong user", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(systemCtx(), testID(1), testID(99))
		if err == nil {
			t.Fatal("expected not-found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if resolvedStatus(appErr) != 404 {
			t.Errorf("expected 404, got %d", resolvedStatus(appErr))
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		store.getErr = fmt.Errorf("storage unavailable")
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(systemCtx(), testID(1), testID(10))
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || resolvedStatus(appErr) != 500 {
			t.Fatalf("expected 500 storage error, got %v", err)
		}
	})

	t.Run("missing blob is not found", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		store.getErr = fmt.Errorf("%w: 10/abc.pdf", storage.ErrNotFound)
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{
			ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(systemCtx(), testID(1), testID(10))
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || resolvedStatus(appErr) != 404 {
			t.Fatalf("expected 404 for a missing blob, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestUploadList(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[testID(1)] = &File{ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "a.txt", StoragePath: "10/a.txt", MimeType: "text/plain", Size: 5}
		repo.files[testID(2)] = &File{ID: testID(2), TenantID: testID(1), UserID: testID(10), OriginalName: "b.txt", StoragePath: "10/b.txt", MimeType: "text/plain", Size: 8}
		repo.files[testID(3)] = &File{ID: testID(3), TenantID: testID(1), UserID: testID(20), OriginalName: "c.txt", StoragePath: "20/c.txt", MimeType: "text/plain", Size: 3}
		repo.nextID = 4

		files, hasMore, err := svc.List(systemCtx(), testID(10), 10, "", false)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if hasMore {
			t.Errorf("expected hasMore=false for a full page")
		}
		if len(files) != 2 {
			t.Errorf("expected 2 files, got %d", len(files))
		}
	})
}
