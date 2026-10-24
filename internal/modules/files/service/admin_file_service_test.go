package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type fakeAdminFileRepo struct {
	filesrepo.FileRepository // unused methods fail loudly if called
	files                    map[int64]*filesqlc.File
	nextID                   int64
}

func (m *fakeAdminFileRepo) scoped(ctx context.Context, f *filesqlc.File) bool {
	return f.TenantID == tenant.EffectiveID(ctx)
}

func newFakeAdminFileRepo() *fakeAdminFileRepo {
	return &fakeAdminFileRepo{files: make(map[int64]*filesqlc.File), nextID: 1}
}

func (m *fakeAdminFileRepo) GetByID(ctx context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok || f.DeletedAt.Valid || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *fakeAdminFileRepo) GetByIDIncludingDeleted(ctx context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (m *fakeAdminFileRepo) AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]filesqlc.File, error) {
	all := make([]filesqlc.File, 0, len(m.files))
	for _, f := range m.files {
		if m.scoped(ctx, f) {
			all = append(all, *f)
		}
	}
	if len(all) > int(cur.Limit) {
		all = all[:cur.Limit]
	}
	return all, nil
}

func (m *fakeAdminFileRepo) Delete(ctx context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok || f.DeletedAt.Valid || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return f, nil
}

func (m *fakeAdminFileRepo) Restore(ctx context.Context, id int64) (*filesqlc.File, error) {
	f, ok := m.files[id]
	if !ok || !f.DeletedAt.Valid || !m.scoped(ctx, f) {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{}
	return f, nil
}

func (m *fakeAdminFileRepo) AdminFileStats(ctx context.Context) (filesqlc.AdminFileStatsRow, error) {
	var stats filesqlc.AdminFileStatsRow
	for _, f := range m.files {
		if f.DeletedAt.Valid || !m.scoped(ctx, f) {
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
			TenantID:     1,
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
		svc := NewAdminFileService(repo, nil, audit.Noop{}, newFakeAdminStorage())
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
		svc := NewAdminFileService(newFakeAdminFileRepo(), nil, audit.Noop{}, newFakeAdminStorage())

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
	svc := NewAdminFileService(repo, nil, audit.Noop{}, store)
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
	svc := NewAdminFileService(repo, nil, audit.Noop{}, newFakeAdminStorage())
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
	svc := NewAdminFileService(repo, nil, audit.Noop{}, newFakeAdminStorage())
	seedAdminFiles(repo, 1)

	_, err := svc.RestoreFile(context.Background(), 1)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Code != string(filescatalog.CodeFileNotDeleted) {
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
	svc := NewAdminFileService(repo, nil, audit.Noop{}, newFakeAdminStorage())
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

func TestAdminFileService_TenantIsolation(t *testing.T) {
	repo := newFakeAdminFileRepo()
	store := newFakeAdminStorage()
	svc := NewAdminFileService(repo, nil, audit.Noop{}, store)
	seedAdminFiles(repo, 1) // tenant 1, file 1
	repo.files[2] = &filesqlc.File{
		ID:           2,
		TenantID:     2,
		UserID:       2,
		OriginalName: "tenant-two.txt",
		StoragePath:  "2/2/file.txt",
		MimeType:     "text/plain",
		Size:         2048,
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	repo.nextID = 3
	store.data["uploads/file.txt"] = []byte("tenant-one")
	store.data["2/2/file.txt"] = []byte("tenant-two")

	ctxA := tenantCtx(1)
	ctxB := tenantCtx(2)

	files, _, err := svc.ListFiles(ctxA, 10, "")
	if err != nil {
		t.Fatalf("tenant one list: %v", err)
	}
	if len(files) != 1 || files[0].ID != 1 || files[0].TenantID != 1 {
		t.Fatalf("tenant one list = %+v, want only file 1", files)
	}

	_, _, err = svc.Download(ctxA, 2)
	requireNotFound(t, err)
	requireNotFound(t, svc.DeleteFile(ctxA, 2))
	if repo.files[2].DeletedAt.Valid {
		t.Fatal("cross-tenant admin delete must not touch tenant two's file")
	}

	repo.files[2].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	_, err = svc.RestoreFile(ctxA, 2)
	requireNotFound(t, err)
	if !repo.files[2].DeletedAt.Valid {
		t.Fatal("cross-tenant admin restore must not revive tenant two's file")
	}

	statsA, err := svc.GetFileStats(ctxA)
	if err != nil {
		t.Fatalf("tenant one stats: %v", err)
	}
	statsB, err := svc.GetFileStats(ctxB)
	if err != nil {
		t.Fatalf("tenant two stats: %v", err)
	}
	if statsA.TotalFiles != 1 || statsA.TotalFileSize != 1024 {
		t.Fatalf("tenant one stats = %+v, want 1 file / 1024 bytes", statsA)
	}
	if statsB.TotalFiles != 0 || statsB.TotalFileSize != 0 {
		t.Fatalf("tenant two active stats = %+v, want zero while file is deleted", statsB)
	}

	if _, err := svc.RestoreFile(ctxB, 2); err != nil {
		t.Fatalf("owning tenant admin restore: %v", err)
	}
	file, reader, err := svc.Download(ctxB, 2)
	if err != nil {
		t.Fatalf("owning tenant admin download: %v", err)
	}
	_ = reader.Close()
	if file.TenantID != 2 {
		t.Fatalf("download tenant = %d, want 2", file.TenantID)
	}
}
