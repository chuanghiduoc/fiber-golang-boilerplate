package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

func newTestUploadService(repo *mockFileRepo, store *mockStorage) UploadService {
	return NewUploadService(repo, store)
}

// ---------------------------------------------------------------------------
// Upload
// ---------------------------------------------------------------------------

func TestUpload(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		resp, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("image-data"), 10, "image/jpeg")
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
		if resp.URL == "" {
			t.Error("expected non-empty URL")
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

		_, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
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
		failRepo := &failingFileRepo{mockFileRepo: newMockFileRepo(), failCreate: true}
		svc := NewUploadService(failRepo, store)

		_, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
		if err == nil {
			t.Fatal("expected error for DB failure")
		}
		// Storage should be cleaned up
		if len(store.files) != 0 {
			t.Error("storage should be cleaned up after DB failure")
		}
	})
}

// failingFileRepo wraps mockFileRepo but can fail on specific operations
type failingFileRepo struct {
	*mockFileRepo
	failCreate bool
}

func (r *failingFileRepo) Create(_ context.Context, _ filesqlc.CreateFileParams) (*filesqlc.File, error) {
	if r.failCreate {
		return nil, fmt.Errorf("db error")
	}
	return r.mockFileRepo.Create(context.Background(), filesqlc.CreateFileParams{})
}

// ---------------------------------------------------------------------------
// GetFileInfo
// ---------------------------------------------------------------------------

func TestGetFileInfo(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		resp, err := svc.GetFileInfo(systemCtx(), 1, 10)
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

		_, err := svc.GetFileInfo(systemCtx(), 999, 10)
		if err == nil {
			t.Fatal("expected not found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Status != 404 {
			t.Errorf("expected 404, got %d", appErr.Status)
		}
	})

	t.Run("forbidden - wrong user", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, err := svc.GetFileInfo(systemCtx(), 1, 99) // wrong user
		if err == nil {
			t.Fatal("expected not-found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Status != 404 {
			t.Errorf("expected 404, got %d", appErr.Status)
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

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		err := svc.Delete(systemCtx(), 1, 10)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		// Soft delete: file stays in repo but has DeletedAt set
		f := repo.files[1]
		if !f.DeletedAt.Valid {
			t.Error("expected DeletedAt to be set (soft delete)")
		}
	})

	t.Run("not found", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		err := svc.Delete(systemCtx(), 999, 10)
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

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		err := svc.Delete(systemCtx(), 1, 99) // wrong user
		if err == nil {
			t.Fatal("expected not-found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Status != 404 {
			t.Errorf("expected 404, got %d", appErr.Status)
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

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}
		store.files["10/abc.pdf"] = []byte("pdf-content")

		file, reader, err := svc.Download(systemCtx(), 1, 10)
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

		_, _, err := svc.Download(systemCtx(), 999, 10)
		if err == nil {
			t.Fatal("expected not found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Status != 404 {
			t.Errorf("expected 404, got %d", appErr.Status)
		}
	})

	t.Run("forbidden - wrong user", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(systemCtx(), 1, 99)
		if err == nil {
			t.Fatal("expected not-found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Status != 404 {
			t.Errorf("expected 404, got %d", appErr.Status)
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		store.getErr = fmt.Errorf("storage unavailable")
		svc := newTestUploadService(repo, store)

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(systemCtx(), 1, 10)
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || appErr.Status != 500 {
			t.Fatalf("expected 500 storage error, got %v", err)
		}
	})

	t.Run("missing blob is not found", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		store.getErr = fmt.Errorf("%w: 10/abc.pdf", storage.ErrNotFound)
		svc := newTestUploadService(repo, store)

		repo.files[1] = &filesqlc.File{
			ID: 1, TenantID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(systemCtx(), 1, 10)
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || appErr.Status != 404 {
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

		repo.files[1] = &filesqlc.File{ID: 1, TenantID: 1, UserID: 10, OriginalName: "a.txt", StoragePath: "10/a.txt", MimeType: "text/plain", Size: 5}
		repo.files[2] = &filesqlc.File{ID: 2, TenantID: 1, UserID: 10, OriginalName: "b.txt", StoragePath: "10/b.txt", MimeType: "text/plain", Size: 8}
		repo.files[3] = &filesqlc.File{ID: 3, TenantID: 1, UserID: 20, OriginalName: "c.txt", StoragePath: "20/c.txt", MimeType: "text/plain", Size: 3}
		repo.nextID = 4

		files, hasMore, err := svc.List(systemCtx(), 10, 10, "", false)
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
