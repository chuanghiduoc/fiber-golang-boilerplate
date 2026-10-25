package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeIdentityAdmin struct {
	users    []User
	hasMore  bool
	roleUser User
	stats    UserStats
	err      error
}

func (f *fakeIdentityAdmin) ListUsers(context.Context, int, string) ([]User, bool, error) {
	return f.users, f.hasMore, f.err
}

func (f *fakeIdentityAdmin) UpdateRole(_ context.Context, _ int64, role string) (*User, error) {
	user := f.roleUser
	user.Role = role
	return &user, f.err
}

func (f *fakeIdentityAdmin) BanUser(context.Context, int64) error { return f.err }

func (f *fakeIdentityAdmin) UnbanUser(context.Context, int64) (*User, error) {
	return &f.roleUser, f.err
}

func (f *fakeIdentityAdmin) GetUserStats(context.Context) (UserStats, error) {
	return f.stats, f.err
}

type fakeFilesAdmin struct {
	files    []File
	hasMore  bool
	download *File
	reader   io.ReadCloser
	restored *File
	stats    FileStats
	err      error
}

func (f *fakeFilesAdmin) ListFiles(context.Context, int, string) ([]File, bool, error) {
	return f.files, f.hasMore, f.err
}

func (f *fakeFilesAdmin) OpenFile(context.Context, int64) (*File, io.ReadCloser, error) {
	return f.download, f.reader, f.err
}

func (f *fakeFilesAdmin) DeleteFile(context.Context, int64) error { return f.err }

func (f *fakeFilesAdmin) RestoreFile(context.Context, int64) (*File, error) {
	return f.restored, f.err
}

func (f *fakeFilesAdmin) GetFileStats(context.Context) (FileStats, error) {
	return f.stats, f.err
}

func newTestService(identity *fakeIdentityAdmin, files *fakeFilesAdmin) Service {
	return NewService(identity, files)
}

func TestListUsers(t *testing.T) {
	now := time.Now()
	deleted := now.Add(-time.Hour)
	svc := newTestService(&fakeIdentityAdmin{
		users:   []User{{ID: 7, Email: "u@example.com", Name: "U", Role: RoleUser, DeletedAt: &deleted}},
		hasMore: true,
	}, &fakeFilesAdmin{})

	users, hasMore, err := svc.ListUsers(context.Background(), 20, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasMore {
		t.Error("HasMore should be preserved")
	}
	if len(users) != 1 || users[0].ID != 7 || users[0].DeletedAt == nil {
		t.Fatalf("users = %+v", users)
	}
}

func TestUpdateRoleRejectsUnknownRole(t *testing.T) {
	svc := newTestService(&fakeIdentityAdmin{}, &fakeFilesAdmin{})

	_, err := svc.UpdateRole(context.Background(), 1, "superuser")
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 400 {
		t.Fatalf("expected bad request, got %v", err)
	}
}

func TestUpdateRoleReturnsOwnerUser(t *testing.T) {
	svc := newTestService(&fakeIdentityAdmin{roleUser: User{ID: 1, Role: RoleUser}}, &fakeFilesAdmin{})

	user, err := svc.UpdateRole(context.Background(), 1, RoleAdmin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user.Role != RoleAdmin || user.ID != 1 {
		t.Fatalf("user = %+v", user)
	}
}

func TestListFiles(t *testing.T) {
	now := time.Now()
	deleted := now.Add(-time.Hour)
	svc := newTestService(&fakeIdentityAdmin{}, &fakeFilesAdmin{
		files: []File{
			{ID: 1, OriginalName: "active.txt", CreatedAt: now},
			{ID: 2, OriginalName: "deleted.txt", CreatedAt: now, DeletedAt: &deleted},
		},
		hasMore: true,
	})

	files, hasMore, err := svc.ListFiles(context.Background(), 20, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasMore {
		t.Error("HasMore should be preserved")
	}
	if len(files) != 2 || files[1].DeletedAt == nil {
		t.Fatalf("files = %+v", files)
	}
}

func TestDownloadReturnsOwnerFileAndReader(t *testing.T) {
	svc := newTestService(&fakeIdentityAdmin{}, &fakeFilesAdmin{
		download: &File{ID: 3, OriginalName: "f.bin"},
		reader:   io.NopCloser(bytes.NewReader([]byte("abc"))),
	})

	file, reader, err := svc.Download(context.Background(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if file.ID != 3 {
		t.Errorf("file = %+v", file)
	}
	data, _ := io.ReadAll(reader)
	if string(data) != "abc" {
		t.Errorf("payload = %q", data)
	}
}

func TestRestoreFileReturnsOwnerFile(t *testing.T) {
	svc := newTestService(&fakeIdentityAdmin{}, &fakeFilesAdmin{
		restored: &File{ID: 4, OriginalName: "r.txt"},
	})

	file, err := svc.RestoreFile(context.Background(), 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if file.ID != 4 || file.DeletedAt != nil {
		t.Fatalf("file = %+v", file)
	}
}

func TestGetStatsComposesOwnerQueries(t *testing.T) {
	svc := newTestService(
		&fakeIdentityAdmin{stats: UserStats{ActiveUsers: 10, DeletedUsers: 2}},
		&fakeFilesAdmin{stats: FileStats{TotalFiles: 5, TotalFileSize: 50000}},
	)

	stats, err := svc.GetStats(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Stats{ActiveUsers: 10, DeletedUsers: 2, TotalFiles: 5, TotalFileSize: 50000}
	if *stats != want {
		t.Errorf("stats = %+v, want %+v", *stats, want)
	}
}

func TestGetStatsWrapsOwnerFailure(t *testing.T) {
	svc := newTestService(&fakeIdentityAdmin{err: apperror.NewInternal("boom")}, &fakeFilesAdmin{})

	_, err := svc.GetStats(context.Background())
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 500 || appErr.Detail != "failed to get system stats" {
		t.Fatalf("expected stable system stats error, got %v", err)
	}
}
