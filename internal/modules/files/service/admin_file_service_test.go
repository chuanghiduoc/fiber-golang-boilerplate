package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type fakeAdminFileRepo struct {
	filesrepo.FileRepository // unused methods fail loudly if called
	files                    map[int64]*filesqlc.File
	nextID                   int64
}

func newFakeAdminFileRepo() *fakeAdminFileRepo {
	return &fakeAdminFileRepo{files: make(map[int64]*filesqlc.File), nextID: 1}
}

func (m *fakeAdminFileRepo) GetByID(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok || f.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *fakeAdminFileRepo) GetByIDIncludingDeleted(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *fakeAdminFileRepo) AdminListCursor(_ context.Context, cur pagination.DBCursor) ([]filesqlc.File, error) {
	all := make([]filesqlc.File, 0, len(m.files))
	for _, f := range m.files {
		all = append(all, *f)
	}
	if len(all) > int(cur.Limit) {
		all = all[:cur.Limit]
	}
	return all, nil
}

func (m *fakeAdminFileRepo) Delete(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok || f.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return f, nil
}

func (m *fakeAdminFileRepo) Restore(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok || !f.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{}
	return f, nil
}

func (m *fakeAdminFileRepo) AdminFileStats(_ context.Context) (filesqlc.AdminFileStatsRow, error) {
	var stats filesqlc.AdminFileStatsRow
	for _, f := range m.files {
		if f.DeletedAt.Valid {
			continue
		}
		stats.TotalFiles++
		stats.TotalFileSize += f.Size
	}
	return stats, nil
}

type fakeAdminStorage struct {
	storage.Storage // unused methods fail loudly if called
	data            map[string][]byte
	getErr          error
}

func newFakeAdminStorage() *fakeAdminStorage {
	return &fakeAdminStorage{data: make(map[string][]byte)}
}

func (s *fakeAdminStorage) Get(_ context.Context, path string) (io.ReadCloser, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	data, ok := s.data[path]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func seedAdminFiles(repo *fakeAdminFileRepo, n int) {
	for i := 0; i < n; i++ {
		repo.files[repo.nextID] = &filesqlc.File{
			ID:           repo.nextID,
			UserID:       1,
			OriginalName: "file.txt",
			StoragePath:  "uploads/file.txt",
			MimeType:     "text/plain",
			Size:         1024,
			CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		repo.nextID++
	}
}

func TestAdminListFiles(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newFakeAdminFileRepo()
		svc := NewAdminFileService(repo, newFakeAdminStorage())
		seedAdminFiles(repo, 2)
		repo.files[2].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}

		files, hasMore, err := svc.ListFiles(context.Background(), 10, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hasMore {
			t.Errorf("expected hasMore=false for a full page")
		}
		if len(files) != 2 {
			t.Fatalf("len(files) = %d, want 2", len(files))
		}
		for _, f := range files {
			if f.ID == 2 {
				if f.URL != "" {
					t.Error("deleted file must not expose a download URL")
				}
				continue
			}
			if f.URL != "/api/v1/admin/files/1/download" {
				t.Errorf("URL = %q, want admin download URL", f.URL)
			}
		}
	})

	t.Run("empty list", func(t *testing.T) {
		svc := NewAdminFileService(newFakeAdminFileRepo(), newFakeAdminStorage())

		files, hasMore, err := svc.ListFiles(context.Background(), 10, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hasMore {
			t.Errorf("expected hasMore=false for an empty list")
		}
		if len(files) != 0 {
			t.Errorf("len(files) = %d, want 0", len(files))
		}
	})
}

func TestAdminDownloadFile(t *testing.T) {
	repo := newFakeAdminFileRepo()
	store := newFakeAdminStorage()
	svc := NewAdminFileService(repo, store)
	seedAdminFiles(repo, 1)
	store.data["uploads/file.txt"] = []byte("payload")

	file, reader, err := svc.Download(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if file.URL != "/api/v1/admin/files/1/download" {
		t.Errorf("URL = %q, want admin download URL", file.URL)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "payload" {
		t.Errorf("payload = %q, want payload", data)
	}
}

func TestAdminDeleteFile(t *testing.T) {
	repo := newFakeAdminFileRepo()
	svc := NewAdminFileService(repo, newFakeAdminStorage())
	seedAdminFiles(repo, 1)

	if err := svc.DeleteFile(context.Background(), 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !repo.files[1].DeletedAt.Valid {
		t.Error("file should be soft-deleted")
	}
	if err := svc.DeleteFile(context.Background(), 999); err == nil {
		t.Fatal("expected not found error")
	} else {
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || appErr.Status != 404 {
			t.Fatalf("expected not found, got %v", err)
		}
	}
}

func TestAdminRestoreFile(t *testing.T) {
	repo := newFakeAdminFileRepo()
	svc := NewAdminFileService(repo, newFakeAdminStorage())
	seedAdminFiles(repo, 1)

	_, err := svc.RestoreFile(context.Background(), 1)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Code != string(apperror.CodeFileNotDeleted) {
		t.Fatalf("expected file.not_deleted, got %v", err)
	}

	repo.files[1].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	file, err := svc.RestoreFile(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if file.DeletedAt != nil {
		t.Error("restored file must not expose deletedAt")
	}
	if file.URL != "/api/v1/admin/files/1/download" {
		t.Errorf("URL = %q, want admin download URL", file.URL)
	}
}

func TestAdminGetFileStats(t *testing.T) {
	repo := newFakeAdminFileRepo()
	svc := NewAdminFileService(repo, newFakeAdminStorage())
	seedAdminFiles(repo, 3)
	repo.files[2].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}

	stats, err := svc.GetFileStats(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2", stats.TotalFiles)
	}
	if stats.TotalFileSize != 2048 {
		t.Errorf("TotalFileSize = %d, want 2048", stats.TotalFileSize)
	}
}
