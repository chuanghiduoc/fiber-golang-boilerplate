package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// memberSubject carries the files permissions a member session resolves to.
// Authorization now reads only the explicit set, never the role name.
func memberSubject(userID, tenantID uuid.UUID) authz.Subject {
	return authz.NewSubjectWithPermissions(userID, tenantID, authz.RoleMember,
		filescatalog.PermissionFilesRead,
		filescatalog.PermissionFilesWrite,
		filescatalog.PermissionFilesDelete,
		filescatalog.PermissionFilesRestore,
	)
}

func adminSubject(userID, tenantID uuid.UUID) authz.Subject {
	return authz.NewSubjectWithPermissions(userID, tenantID, authz.RoleAdmin,
		filescatalog.PermissionFilesRead,
		filescatalog.PermissionFilesWrite,
		filescatalog.PermissionFilesDelete,
		filescatalog.PermissionFilesRestore,
		authz.PermissionAdminFilesRead,
		authz.PermissionAdminFilesDelete,
		authz.PermissionAdminFilesRestore,
		authz.PermissionAdminStatsRead,
	)
}

func abacCtx(tenantID uuid.UUID, subject authz.Subject) context.Context {
	return authz.WithSubject(tenant.WithID(context.Background(), tenantID), subject)
}

func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != status {
		t.Fatalf("error = %v, want status %d", err, status)
	}
}

func seedABACFiles(repo *mockFileRepo, store *mockStorage) {
	repo.files[testID(1)] = &File{
		ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "own.pdf",
		StoragePath: "1/10/own.pdf", MimeType: "application/pdf", Size: 10,
		CreatedAt: time.Now(),
	}
	repo.files[testID(2)] = &File{
		ID: testID(2), TenantID: testID(2), UserID: testID(20), OriginalName: "other-tenant.pdf",
		StoragePath: "2/20/other.pdf", MimeType: "application/pdf", Size: 20,
		CreatedAt: time.Now(),
	}
	repo.files[testID(3)] = &File{
		ID: testID(3), TenantID: testID(1), UserID: testID(11), OriginalName: "same-tenant-other-owner.pdf",
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
	svc := NewUploadService(repo, newFakeTx(repo), store, testAuthorizer(), UploadOptions{})

	ctx := abacCtx(testID(1), memberSubject(testID(10), testID(1)))

	if _, err := svc.GetFileInfo(ctx, testID(1), testID(10)); err != nil {
		t.Fatalf("owner get: %v", err)
	}
	_, err := svc.GetFileInfo(ctx, testID(2), testID(20))
	requireStatus(t, err, 404)
	_, err = svc.GetFileInfo(ctx, testID(3), testID(11))
	requireStatus(t, err, 404)

	_, _, err = svc.Download(ctx, testID(2), testID(20))
	requireStatus(t, err, 404)
	_, _, err = svc.Download(ctx, testID(3), testID(11))
	requireStatus(t, err, 404)

	requireStatus(t, svc.Delete(ctx, testID(2), testID(20)), 404)
	if repo.files[testID(2)].DeletedAt != nil {
		t.Fatal("ABAC must prevent a cross-tenant delete")
	}
	requireStatus(t, svc.Delete(ctx, testID(3), testID(11)), 404)
	if repo.files[testID(3)].DeletedAt != nil {
		t.Fatal("ABAC must prevent deleting another user's file")
	}

	repo.files[testID(2)].DeletedAt = timePtr(time.Now())
	_, err = svc.Restore(ctx, testID(2), testID(20))
	requireStatus(t, err, 404)
	if repo.files[testID(2)].DeletedAt == nil {
		t.Fatal("ABAC must not revive a cross-tenant file")
	}

	// The owning tenant/user path still works.
	repo.files[testID(3)].DeletedAt = timePtr(time.Now())
	ownerCtx := abacCtx(testID(1), memberSubject(testID(11), testID(1)))
	if _, err := svc.Restore(ownerCtx, testID(3), testID(11)); err != nil {
		t.Fatalf("owner restore: %v", err)
	}
}

func TestUploadService_AdminPermissionsWidenToTheTenant(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	seedABACFiles(repo, store)
	svc := NewUploadService(repo, newFakeTx(repo), store, testAuthorizer(), UploadOptions{})

	admin := authz.NewSubjectWithPermissions(testID(99), testID(1), authz.RoleAdmin,
		authz.Permission("files:read"),
		authz.Permission("files:delete"),
		authz.Permission("files:restore"),
		authz.PermissionAdminFilesRead,
		authz.PermissionAdminFilesDelete,
		authz.PermissionAdminFilesRestore,
	)
	ctx := abacCtx(testID(1), admin)

	files, _, err := svc.List(ctx, testID(99), 10, "", false)
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("admin list = %d files, want the tenant's 2 files", len(files))
	}
	for _, file := range files {
		if file.TenantID != testID(1) {
			t.Fatalf("admin list leaked tenant %s", file.TenantID)
		}
	}

	if _, err := svc.GetFileInfo(ctx, testID(3), testID(99)); err != nil {
		t.Fatalf("admin get another owner's file: %v", err)
	}
	_, reader, err := svc.Download(ctx, testID(3), testID(99))
	if err != nil {
		t.Fatalf("admin download another owner's file: %v", err)
	}
	_ = reader.Close()

	// The tenant boundary still wins over an admin permission.
	_, err = svc.GetFileInfo(ctx, testID(2), testID(99))
	requireStatus(t, err, 404)
	requireStatus(t, svc.Delete(ctx, testID(2), testID(99)), 404)

	if err := svc.Delete(ctx, testID(3), testID(99)); err != nil {
		t.Fatalf("admin delete another owner's file: %v", err)
	}
	if repo.files[testID(3)].DeletedAt == nil {
		t.Fatal("admin delete did not soft-delete the file")
	}
	visible, _, err := svc.List(ctx, testID(99), 10, "", false)
	if err != nil {
		t.Fatalf("admin list after delete: %v", err)
	}
	if len(visible) != 1 {
		t.Fatalf("admin list without includeDeleted = %d files, want 1", len(visible))
	}
	trash, _, err := svc.List(ctx, testID(99), 10, "", true)
	if err != nil {
		t.Fatalf("admin list including deleted: %v", err)
	}
	if len(trash) != 2 {
		t.Fatalf("admin list with includeDeleted = %d files, want 2", len(trash))
	}
	if _, err := svc.Restore(ctx, testID(3), testID(99)); err != nil {
		t.Fatalf("admin restore another owner's file: %v", err)
	}
}

func TestUploadService_ABACPermissionPreCheck(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	seedABACFiles(repo, store)
	svc := NewUploadService(repo, newFakeTx(repo), store, testAuthorizer(), UploadOptions{})

	// A JWT-equivalent subject whose role grants files:read.
	ctx := abacCtx(testID(1), memberSubject(testID(10), testID(1)))
	if _, err := svc.GetFileInfo(ctx, testID(1), testID(10)); err != nil {
		t.Fatalf("allowed read: %v", err)
	}

	// An explicit API-key scope set without files:read must fail the RBAC
	// pre-check with 403 even though the owner/tenant policies would allow.
	noScopes := abacCtx(testID(1), authz.NewSubjectWithPermissions(testID(10), testID(1), authz.RoleAdmin))
	_, err := svc.GetFileInfo(noScopes, testID(1), testID(10))
	requireStatus(t, err, 403)

	_, _, err = svc.List(noScopes, testID(10), 10, "", false)
	requireStatus(t, err, 403)
}

func TestAdminFileService_ABACTenantPolicy(t *testing.T) {
	repo := newFakeAdminFileRepo()
	store := newFakeAdminStorage()
	seedAdminFiles(repo, 1)
	repo.files[testID(2)] = &File{
		ID: testID(2), TenantID: testID(2), UserID: testID(20), OriginalName: "tenant-two.pdf",
		StoragePath: "2/20/two.pdf", MimeType: "application/pdf", Size: 20,
		CreatedAt: time.Now(),
	}
	repo.files[testID(3)] = &File{
		ID: testID(3), TenantID: testID(1), UserID: testID(30), OriginalName: "same-tenant.pdf",
		StoragePath: "1/30/three.pdf", MimeType: "application/pdf", Size: 30,
		CreatedAt: time.Now(),
	}
	repo.nextID = 4
	store.data["uploads/file.txt"] = []byte("tenant-one")
	store.data["2/20/two.pdf"] = []byte("tenant-two")

	svc := NewAdminFileService(repo, newFakeTx(repo), store, testAuthorizer())
	ctx := abacCtx(testID(1), adminSubject(testID(1), testID(1)))

	files, _, err := svc.ListFiles(ctx, 10, "")
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	for _, file := range files {
		if file.TenantID != testID(1) {
			t.Fatalf("admin list leaked tenant %s", file.TenantID)
		}
	}

	_, _, err = svc.Download(ctx, testID(2))
	requireStatus(t, err, 404)
	requireStatus(t, svc.DeleteFile(ctx, testID(2)), 404)
	if repo.files[testID(2)].DeletedAt != nil {
		t.Fatal("ABAC must prevent cross-tenant admin delete")
	}

	repo.files[testID(2)].DeletedAt = timePtr(time.Now())
	_, err = svc.RestoreFile(ctx, testID(2))
	requireStatus(t, err, 404)
	if repo.files[testID(2)].DeletedAt == nil {
		t.Fatal("ABAC must prevent cross-tenant admin restore")
	}

	// A tenant-1 admin may manage tenant-1 files, including another owner's.
	if err := svc.DeleteFile(ctx, testID(3)); err != nil {
		t.Fatalf("same-tenant admin delete: %v", err)
	}

	// An explicit API-key scope set without admin:files:read fails the RBAC
	// pre-check.
	noScopes := abacCtx(testID(1), authz.NewSubjectWithPermissions(testID(1), testID(1), authz.RoleAdmin))
	_, _, err = svc.ListFiles(noScopes, 10, "")
	requireStatus(t, err, 403)
	_, _, err = svc.Download(noScopes, testID(1))
	requireStatus(t, err, 403)
	_, err = svc.GetFileStats(noScopes)
	requireStatus(t, err, 403)
}

// systemCtx is the trusted background-caller context. Tests that exercise
// service mechanics rather than authorization use it, because a context with
// neither a subject nor the system marker is denied. It names the seeded
// default tenant explicitly: there is no tenant fallback.
func systemCtx() context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), tenant.DefaultID))
}

func TestFileServices_NoSubjectNoSystemMarkerIsDenied(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	seedABACFiles(repo, store)
	svc := NewUploadService(repo, newFakeTx(repo), store, testAuthorizer(), UploadOptions{})
	ctx := tenant.WithID(context.Background(), testID(1))

	_, err := svc.Upload(ctx, testID(10), "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
	requireStatus(t, err, 403)
	if repo.nextID != 4 {
		t.Fatal("a denied upload must not create a file row")
	}
	_, err = svc.GetFileInfo(ctx, testID(1), testID(10))
	requireStatus(t, err, 403)
	_, _, err = svc.Download(ctx, testID(1), testID(10))
	requireStatus(t, err, 403)
	_, _, err = svc.List(ctx, testID(10), 10, "", false)
	requireStatus(t, err, 403)
	requireStatus(t, svc.Delete(ctx, testID(1), testID(10)), 403)
	if repo.files[testID(1)].DeletedAt != nil {
		t.Fatal("a denied delete must not soft-delete the file")
	}

	adminRepo := newFakeAdminFileRepo()
	seedAdminFiles(adminRepo, 1)
	admin := NewAdminFileService(adminRepo, newFakeTx(adminRepo), newFakeAdminStorage(), testAuthorizer())
	_, _, err = admin.ListFiles(ctx, 10, "")
	requireStatus(t, err, 403)
	requireStatus(t, admin.DeleteFile(ctx, testID(1)), 403)
	_, err = admin.GetFileStats(ctx)
	requireStatus(t, err, 403)
}
