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

func TestListUsersPassesOwnerPage(t *testing.T) {
	page := identitycontracts.UserPage{Items: []identitycontracts.User{{ID: 7, Role: RoleUser}}, HasMore: true}
	got, err := NewService(&fakeIdentityAdmin{users: page}, &fakeFilesAdmin{}).ListUsers(context.Background(), 20, "")
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if !got.HasMore || len(got.Items) != 1 || got.Items[0].ID != 7 {
		t.Fatalf("ListUsers = %+v, want the owner page", got)
	}
}

func TestUpdateRoleRejectsUnknownRole(t *testing.T) {
	_, err := NewService(&fakeIdentityAdmin{}, &fakeFilesAdmin{}).UpdateRole(context.Background(), 1, "owner")
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 400 {
		t.Fatalf("UpdateRole(owner) error = %v, want 400", err)
	}
}

func TestUpdateRoleReturnsOwnerUser(t *testing.T) {
	svc := NewService(&fakeIdentityAdmin{roleUser: identitycontracts.User{ID: 1}}, &fakeFilesAdmin{})
	user, err := svc.UpdateRole(context.Background(), 1, RoleAdmin)
	if err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	if user.ID != 1 || user.Role != RoleAdmin {
		t.Fatalf("UpdateRole = %+v, want user 1 with role admin", user)
	}
}

func TestDownloadPassesFileAndReader(t *testing.T) {
	reader := io.NopCloser(bytes.NewReader([]byte("data")))
	file := filescontracts.File{ID: 3, MimeType: "image/png", Size: 4, CreatedAt: time.Now()}
	svc := NewService(&fakeIdentityAdmin{}, &fakeFilesAdmin{download: filescontracts.FileDownload{File: file, Reader: reader}})
	got, err := svc.Download(context.Background(), 3)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got.File.ID != 3 || got.Reader != reader {
		t.Fatalf("Download = %+v, want the owner file and reader", got)
	}
}

func TestGetStatsComposesOwnerQueries(t *testing.T) {
	svc := NewService(
		&fakeIdentityAdmin{stats: identitycontracts.UserStats{ActiveUsers: 5, DeletedUsers: 2}},
		&fakeFilesAdmin{stats: filescontracts.FileStats{TotalFiles: 9, TotalFileSize: 1024}},
	)
	stats, err := svc.GetStats(context.Background())
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	want := Stats{ActiveUsers: 5, DeletedUsers: 2, TotalFiles: 9, TotalFileSize: 1024}
	if stats != want {
		t.Fatalf("GetStats = %+v, want %+v", stats, want)
	}
}

func TestGetStatsHidesOwnerFailure(t *testing.T) {
	svc := NewService(&fakeIdentityAdmin{}, &fakeFilesAdmin{err: errors.New("db down")})
	_, err := svc.GetStats(context.Background())
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 500 || appErr.Detail != "failed to get system stats" {
		t.Fatalf("GetStats error = %v, want public 500", err)
	}
}
