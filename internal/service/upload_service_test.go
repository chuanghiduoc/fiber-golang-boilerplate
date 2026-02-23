package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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

		resp, err := svc.Upload(context.Background(), 1, "photo.jpg", strings.NewReader("image-data"), 10, "image/jpeg")
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

		_, err := svc.Upload(context.Background(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
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

		_, err := svc.Upload(context.Background(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
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

func (r *failingFileRepo) Create(_ context.Context, _ sqlc.CreateFileParams) (*sqlc.File, error) {
	if r.failCreate {
		return nil, fmt.Errorf("db error")
	}
	return r.mockFileRepo.Create(context.Background(), sqlc.CreateFileParams{})
}

// ---------------------------------------------------------------------------
// GetFileInfo
// ---------------------------------------------------------------------------

func TestGetFileInfo(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[1] = &sqlc.File{
			ID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		resp, err := svc.GetFileInfo(context.Background(), 1, 10)
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

		_, err := svc.GetFileInfo(context.Background(), 999, 10)
		if err == nil {
			t.Fatal("expected not found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Code != 404 {
			t.Errorf("expected 404, got %d", appErr.Code)
		}
	})

	t.Run("forbidden - wrong user", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[1] = &sqlc.File{
			ID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, err := svc.GetFileInfo(context.Background(), 1, 99) // wrong user
		if err == nil {
			t.Fatal("expected forbidden error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Code != 403 {
			t.Errorf("expected 403, got %d", appErr.Code)
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

		repo.files[1] = &sqlc.File{
			ID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		err := svc.Delete(context.Background(), 1, 10)
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

		err := svc.Delete(context.Background(), 999, 10)
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

		repo.files[1] = &sqlc.File{
			ID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		err := svc.Delete(context.Background(), 1, 99) // wrong user
		if err == nil {
			t.Fatal("expected forbidden error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Code != 403 {
			t.Errorf("expected 403, got %d", appErr.Code)
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

		repo.files[1] = &sqlc.File{
			ID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}
		store.files["10/abc.pdf"] = []byte("pdf-content")

		file, reader, err := svc.Download(context.Background(), 1, 10)
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

		_, _, err := svc.Download(context.Background(), 999, 10)
		if err == nil {
			t.Fatal("expected not found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Code != 404 {
			t.Errorf("expected 404, got %d", appErr.Code)
		}
	})

	t.Run("forbidden - wrong user", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		svc := newTestUploadService(repo, store)

		repo.files[1] = &sqlc.File{
			ID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(context.Background(), 1, 99)
		if err == nil {
			t.Fatal("expected forbidden error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Code != 403 {
			t.Errorf("expected 403, got %d", appErr.Code)
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		repo := newMockFileRepo()
		store := newMockStorage()
		store.getErr = fmt.Errorf("storage unavailable")
		svc := newTestUploadService(repo, store)

		repo.files[1] = &sqlc.File{
			ID: 1, UserID: 10, OriginalName: "doc.pdf",
			StoragePath: "10/abc.pdf", MimeType: "application/pdf", Size: 100,
		}

		_, _, err := svc.Download(context.Background(), 1, 10)
		if err == nil {
			t.Fatal("expected storage error")
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

		repo.files[1] = &sqlc.File{ID: 1, UserID: 10, OriginalName: "a.txt", StoragePath: "10/a.txt", MimeType: "text/plain", Size: 5}
		repo.files[2] = &sqlc.File{ID: 2, UserID: 10, OriginalName: "b.txt", StoragePath: "10/b.txt", MimeType: "text/plain", Size: 8}
		repo.files[3] = &sqlc.File{ID: 3, UserID: 20, OriginalName: "c.txt", StoragePath: "20/c.txt", MimeType: "text/plain", Size: 3}
		repo.nextID = 4

		files, total, err := svc.List(context.Background(), 10, 1, 10)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if total != 2 {
			t.Errorf("expected total 2 for user 10, got %d", total)
		}
		if len(files) != 2 {
			t.Errorf("expected 2 files, got %d", len(files))
		}
	})
}
