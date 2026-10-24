package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeIdentityAdmin struct {
	users    identitycontracts.UserPage
	roleUser identitycontracts.User
	stats    identitycontracts.UserStats
	err      error
}

func (f *fakeIdentityAdmin) ListUsers(context.Context, int, string) (identitycontracts.UserPage, error) {
	return f.users, f.err
}

func (f *fakeIdentityAdmin) UpdateRole(_ context.Context, _ int64, role string) (identitycontracts.User, error) {
	user := f.roleUser
	user.Role = role
	return user, f.err
}

func (f *fakeIdentityAdmin) BanUser(context.Context, int64) error { return f.err }

func (f *fakeIdentityAdmin) UnbanUser(context.Context, int64) (identitycontracts.User, error) {
	return f.roleUser, f.err
}

func (f *fakeIdentityAdmin) GetUserStats(context.Context) (identitycontracts.UserStats, error) {
	return f.stats, f.err
}

type fakeFilesAdmin struct {
	files    filescontracts.FilePage
	download filescontracts.FileDownload
	restored filescontracts.File
	stats    filescontracts.FileStats
	err      error
}

func (f *fakeFilesAdmin) ListFiles(context.Context, int, string) (filescontracts.FilePage, error) {
	return f.files, f.err
}

func (f *fakeFilesAdmin) OpenFile(context.Context, int64) (filescontracts.FileDownload, error) {
	return f.download, f.err
}

func (f *fakeFilesAdmin) DeleteFile(context.Context, int64) error { return f.err }

func (f *fakeFilesAdmin) RestoreFile(context.Context, int64) (filescontracts.File, error) {
	return f.restored, f.err
}

func (f *fakeFilesAdmin) GetFileStats(context.Context) (filescontracts.FileStats, error) {
	return f.stats, f.err
}

func newTestService(identity *fakeIdentityAdmin, files *fakeFilesAdmin) Service {
	return NewService(identity, files)
}

func TestListUsers(t *testing.T) {
	now := time.Now()
	deleted := now.Add(-time.Hour)
	svc := newTestService(&fakeIdentityAdmin{users: identitycontracts.UserPage{
		Items: []identitycontracts.User{{
			ID: 7, Email: "u@example.com", Name: "U", Role: admindto.RoleUser,
			EmailVerified: true, HasPassword: true, CreatedAt: now, UpdatedAt: now, DeletedAt: &deleted,
		}},
		HasMore: true,
	}}, &fakeFilesAdmin{})

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

func TestUpdateRoleMapsUser(t *testing.T) {
	now := time.Now()
	svc := newTestService(&fakeIdentityAdmin{roleUser: identitycontracts.User{
		ID: 1, Email: "u@example.com", Name: "U", Role: admindto.RoleUser, CreatedAt: now, UpdatedAt: now,
	}}, &fakeFilesAdmin{})

	user, err := svc.UpdateRole(context.Background(), 1, admindto.RoleAdmin)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user.Role != admindto.RoleAdmin || user.ID != 1 {
		t.Fatalf("user = %+v", user)
	}
}

func TestListFilesMapsAdminURL(t *testing.T) {
	now := time.Now()
	deleted := now.Add(-time.Hour)
	svc := newTestService(&fakeIdentityAdmin{}, &fakeFilesAdmin{files: filescontracts.FilePage{
		Items: []filescontracts.File{
			{ID: 1, OriginalName: "active.txt", CreatedAt: now},
			{ID: 2, OriginalName: "deleted.txt", CreatedAt: now, DeletedAt: &deleted},
		},
	}})

	files, _, err := svc.ListFiles(context.Background(), 20, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if files[0].URL != "/api/v1/admin/files/1/download" {
		t.Errorf("active URL = %q", files[0].URL)
	}
	if files[1].URL != "" || files[1].DeletedAt == nil {
		t.Errorf("deleted file = %+v", files[1])
	}
}

func TestDownloadReturnsReader(t *testing.T) {
	svc := newTestService(&fakeIdentityAdmin{}, &fakeFilesAdmin{download: filescontracts.FileDownload{
		File:   filescontracts.File{ID: 3, OriginalName: "f.bin", MimeType: "application/octet-stream", Size: 3},
		Reader: io.NopCloser(bytes.NewReader([]byte("abc"))),
	}})

	file, reader, err := svc.Download(context.Background(), 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if file.URL != "/api/v1/admin/files/3/download" {
		t.Errorf("URL = %q", file.URL)
	}
	data, _ := io.ReadAll(reader)
	if string(data) != "abc" {
		t.Errorf("payload = %q", data)
	}
}

func TestRestoreFileMapsResponse(t *testing.T) {
	now := time.Now()
	svc := newTestService(&fakeIdentityAdmin{}, &fakeFilesAdmin{restored: filescontracts.File{
		ID: 4, OriginalName: "r.txt", CreatedAt: now,
	}})

	file, err := svc.RestoreFile(context.Background(), 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if file.DeletedAt != nil || file.URL != "/api/v1/admin/files/4/download" {
		t.Fatalf("file = %+v", file)
	}
}

func TestGetStatsComposesOwnerQueries(t *testing.T) {
	svc := newTestService(
		&fakeIdentityAdmin{stats: identitycontracts.UserStats{ActiveUsers: 10, DeletedUsers: 2}},
		&fakeFilesAdmin{stats: filescontracts.FileStats{TotalFiles: 5, TotalFileSize: 50000}},
	)

	stats, err := svc.GetStats(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := admindto.AdminStatsResponse{ActiveUsers: 10, DeletedUsers: 2, TotalFiles: 5, TotalFileSize: 50000}
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
