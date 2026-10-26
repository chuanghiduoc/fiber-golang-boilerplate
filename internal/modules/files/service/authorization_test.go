package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func abacCtx(tenantID int64, subject authz.Subject) context.Context {
	return authz.WithSubject(tenant.WithID(context.Background(), tenantID), subject)
}

func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != status {
		t.Fatalf("error = %v, want status %d", err, status)
	}
}

func seedABACFiles(repo *mockFileRepo, store *mockStorage) {
	repo.files[1] = &File{
		ID: 1, TenantID: 1, UserID: 10, OriginalName: "own.pdf",
		StoragePath: "1/10/own.pdf", MimeType: "application/pdf", Size: 10,
		CreatedAt: time.Now(),
	}
	repo.files[2] = &File{
		ID: 2, TenantID: 2, UserID: 20, OriginalName: "other-tenant.pdf",
		StoragePath: "2/20/other.pdf", MimeType: "application/pdf", Size: 20,
		CreatedAt: time.Now(),
	}
	repo.files[3] = &File{
		ID: 3, TenantID: 1, UserID: 11, OriginalName: "same-tenant-other-owner.pdf",
		StoragePath: "1/11/other.pdf", MimeType: "application/pdf", Size: 30,
		CreatedAt: time.Now(),
	}
	repo.nextID = 4
	store.files["1/10/own.pdf"] = []byte("own")
	store.files["2/20/other.pdf"] = []byte("other")
	store.files["1/11/other.pdf"] = []byte("other-owner")
}

func TestUploadService_ABACOwnerAndTenantPolicies(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	seedABACFiles(repo, store)
	svc := NewUploadService(repo, newFakeTx(repo), store, UploadOptions{})

	ctx := abacCtx(1, authz.NewSubject(10, 1, authz.RoleMember))

	if _, err := svc.GetFileInfo(ctx, 1, 10); err != nil {
		t.Fatalf("owner get: %v", err)
	}
	_, err := svc.GetFileInfo(ctx, 2, 20)
	requireStatus(t, err, 404)
	_, err = svc.GetFileInfo(ctx, 3, 11)
	requireStatus(t, err, 404)

	_, _, err = svc.Download(ctx, 2, 20)
	requireStatus(t, err, 404)
	_, _, err = svc.Download(ctx, 3, 11)
	requireStatus(t, err, 404)

	requireStatus(t, svc.Delete(ctx, 2, 20), 404)
	if repo.files[2].DeletedAt != nil {
		t.Fatal("ABAC must prevent a cross-tenant delete")
	}
	requireStatus(t, svc.Delete(ctx, 3, 11), 404)
	if repo.files[3].DeletedAt != nil {
		t.Fatal("ABAC must prevent deleting another user's file")
	}

	repo.files[2].DeletedAt = timePtr(time.Now())
	_, err = svc.Restore(ctx, 2, 20)
	requireStatus(t, err, 404)
	if repo.files[2].DeletedAt == nil {
		t.Fatal("ABAC must not revive a cross-tenant file")
	}

	// The owning tenant/user path still works.
	repo.files[3].DeletedAt = timePtr(time.Now())
	ownerCtx := abacCtx(1, authz.NewSubject(11, 1, authz.RoleMember))
	if _, err := svc.Restore(ownerCtx, 3, 11); err != nil {
		t.Fatalf("owner restore: %v", err)
	}
}

func TestUploadService_ABACPermissionPreCheck(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	seedABACFiles(repo, store)
	svc := NewUploadService(repo, newFakeTx(repo), store, UploadOptions{})

	// A JWT-equivalent subject whose role grants files:read.
	ctx := abacCtx(1, authz.NewSubject(10, 1, authz.RoleMember))
	if _, err := svc.GetFileInfo(ctx, 1, 10); err != nil {
		t.Fatalf("allowed read: %v", err)
	}

	// An explicit API-key scope set without files:read must fail the RBAC
	// pre-check with 403 even though the owner/tenant policies would allow.
	noScopes := abacCtx(1, authz.NewSubjectWithPermissions(10, 1, authz.RoleAdmin))
	_, err := svc.GetFileInfo(noScopes, 1, 10)
	requireStatus(t, err, 403)

	_, _, err = svc.List(noScopes, 10, 10, "", false)
	requireStatus(t, err, 403)
}

func TestAdminFileService_ABACTenantPolicy(t *testing.T) {
	repo := newFakeAdminFileRepo()
	store := newFakeAdminStorage()
	seedAdminFiles(repo, 1)
	repo.files[2] = &File{
		ID: 2, TenantID: 2, UserID: 20, OriginalName: "tenant-two.pdf",
		StoragePath: "2/20/two.pdf", MimeType: "application/pdf", Size: 20,
		CreatedAt: time.Now(),
	}
	repo.files[3] = &File{
		ID: 3, TenantID: 1, UserID: 30, OriginalName: "same-tenant.pdf",
		StoragePath: "1/30/three.pdf", MimeType: "application/pdf", Size: 30,
		CreatedAt: time.Now(),
	}
	repo.nextID = 4
	store.data["uploads/file.txt"] = []byte("tenant-one")
	store.data["2/20/two.pdf"] = []byte("tenant-two")

	svc := NewAdminFileService(repo, newFakeTx(repo), store)
	ctx := abacCtx(1, authz.NewSubject(1, 1, authz.RoleAdmin))

	files, _, err := svc.ListFiles(ctx, 10, "")
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	for _, file := range files {
		if file.TenantID != 1 {
			t.Fatalf("admin list leaked tenant %d", file.TenantID)
		}
	}

	_, _, err = svc.Download(ctx, 2)
	requireStatus(t, err, 404)
	requireStatus(t, svc.DeleteFile(ctx, 2), 404)
	if repo.files[2].DeletedAt != nil {
		t.Fatal("ABAC must prevent cross-tenant admin delete")
	}

	repo.files[2].DeletedAt = timePtr(time.Now())
	_, err = svc.RestoreFile(ctx, 2)
	requireStatus(t, err, 404)
	if repo.files[2].DeletedAt == nil {
		t.Fatal("ABAC must prevent cross-tenant admin restore")
	}

	// A tenant-1 admin may manage tenant-1 files, including another owner's.
	if err := svc.DeleteFile(ctx, 3); err != nil {
		t.Fatalf("same-tenant admin delete: %v", err)
	}

	// An explicit API-key scope set without admin:files:read fails the RBAC
	// pre-check.
	noScopes := abacCtx(1, authz.NewSubjectWithPermissions(1, 1, authz.RoleAdmin))
	_, _, err = svc.ListFiles(noScopes, 10, "")
	requireStatus(t, err, 403)
	_, _, err = svc.Download(noScopes, 1)
	requireStatus(t, err, 403)
	_, err = svc.GetFileStats(noScopes)
	requireStatus(t, err, 403)
}

// systemCtx is the trusted background-caller context. Tests that exercise
// service mechanics rather than authorization use it, because a context with
// neither a subject nor the system marker is denied.
func systemCtx() context.Context {
	return authz.SystemContext(context.Background())
}

func TestFileServices_NoSubjectNoSystemMarkerIsDenied(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	seedABACFiles(repo, store)
	svc := NewUploadService(repo, newFakeTx(repo), store, UploadOptions{})
	ctx := tenant.WithID(context.Background(), 1)

	_, err := svc.Upload(ctx, 10, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
	requireStatus(t, err, 403)
	if repo.nextID != 4 {
		t.Fatal("a denied upload must not create a file row")
	}
	_, err = svc.GetFileInfo(ctx, 1, 10)
	requireStatus(t, err, 403)
	_, _, err = svc.Download(ctx, 1, 10)
	requireStatus(t, err, 403)
	_, _, err = svc.List(ctx, 10, 10, "", false)
	requireStatus(t, err, 403)
	requireStatus(t, svc.Delete(ctx, 1, 10), 403)
	if repo.files[1].DeletedAt != nil {
		t.Fatal("a denied delete must not soft-delete the file")
	}

	adminRepo := newFakeAdminFileRepo()
	seedAdminFiles(adminRepo, 1)
	admin := NewAdminFileService(adminRepo, newFakeTx(adminRepo), newFakeAdminStorage())
	_, _, err = admin.ListFiles(ctx, 10, "")
	requireStatus(t, err, 403)
	requireStatus(t, admin.DeleteFile(ctx, 1), 403)
	_, err = admin.GetFileStats(ctx)
	requireStatus(t, err, 403)
}
