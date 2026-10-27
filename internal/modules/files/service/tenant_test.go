package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func tenantCtx(id uuid.UUID) context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), id))
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 404 {
		t.Fatalf("error = %v, want 404 not found", err)
	}
}

func TestUpload_StoragePathIsTenantScoped(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := newTestUploadService(repo, store)

	resp, err := svc.Upload(tenantCtx(testID(2)), testID(10), "photo.jpg", strings.NewReader("pdf-data"), 8, "application/pdf")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if resp.TenantID != testID(2) {
		t.Fatalf("response tenant = %s, want %s", resp.TenantID, testID(2))
	}
	if len(store.files) != 1 {
		t.Fatalf("stored blobs = %d, want 1", len(store.files))
	}
	wantPrefix := testID(2).String() + "/" + testID(10).String() + "/"
	for path := range store.files {
		if !strings.HasPrefix(path, wantPrefix) {
			t.Fatalf("storage path = %q, want tenant/user prefix %s", path, wantPrefix)
		}
		if !strings.HasSuffix(path, ".pdf") {
			t.Fatalf("storage path = %q, want .pdf extension", path)
		}
	}
	if repo.files[resp.ID].TenantID != testID(2) {
		t.Fatalf("repo tenant = %s, want %s", repo.files[resp.ID].TenantID, testID(2))
	}

	// The same user id in another tenant gets a different key space.
	other, err := svc.Upload(tenantCtx(testID(3)), testID(10), "photo.jpg", strings.NewReader("pdf-data"), 8, "application/pdf")
	if err != nil {
		t.Fatalf("upload tenant 3: %v", err)
	}
	if other.TenantID != testID(3) {
		t.Fatalf("response tenant = %s, want %s", other.TenantID, testID(3))
	}
	wantOther := testID(3).String() + "/" + testID(10).String() + "/"
	for path := range store.files {
		if strings.HasPrefix(path, wantOther) {
			return
		}
	}
	t.Fatalf("tenant 3 upload did not use the %s key space", wantOther)
}

func TestUpload_RejectsInvalidTenant(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := newTestUploadService(repo, store)

	_, err := svc.Upload(tenantCtx(uuid.Nil), testID(10), "photo.jpg", strings.NewReader("data"), 4, "application/pdf")
	if err == nil {
		t.Fatal("upload with an invalid tenant must fail")
	}
	if len(store.files) != 0 || len(repo.files) != 0 {
		t.Fatal("invalid tenant must not write storage or metadata")
	}
}

func TestUploadService_TenantIsolation(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := newTestUploadService(repo, store)

	repo.files[testID(1)] = &File{
		ID: testID(1), TenantID: testID(1), UserID: testID(10), OriginalName: "tenant-one.pdf",
		StoragePath: "1/10/a.pdf", MimeType: "application/pdf", Size: 10,
	}
	repo.files[testID(2)] = &File{
		ID: testID(2), TenantID: testID(2), UserID: testID(20), OriginalName: "tenant-two.pdf",
		StoragePath: "2/20/b.pdf", MimeType: "application/pdf", Size: 20,
	}
	repo.files[testID(3)] = &File{
		ID: testID(3), TenantID: testID(2), UserID: testID(10), OriginalName: "same-user-other-tenant.pdf",
		StoragePath: "2/10/c.pdf", MimeType: "application/pdf", Size: 30,
	}
	repo.nextID = 4
	store.files["1/10/a.pdf"] = []byte("a")
	store.files["2/20/b.pdf"] = []byte("b")
	store.files["2/10/c.pdf"] = []byte("c")

	ctxA := tenantCtx(testID(1))

	_, err := svc.GetFileInfo(ctxA, testID(2), testID(20))
	requireNotFound(t, err)
	_, err = svc.GetFileInfo(ctxA, testID(3), testID(10))
	requireNotFound(t, err)
	_, _, err = svc.Download(ctxA, testID(2), testID(20))
	requireNotFound(t, err)
	requireNotFound(t, svc.Delete(ctxA, testID(2), testID(20)))
	if repo.files[testID(2)].DeletedAt != nil {
		t.Fatal("cross-tenant delete must not touch tenant two's file")
	}

	repo.files[testID(2)].DeletedAt = timePtr(time.Now())
	_, err = svc.Restore(ctxA, testID(2), testID(20))
	requireNotFound(t, err)
	if repo.files[testID(2)].DeletedAt == nil {
		t.Fatal("cross-tenant restore must not revive tenant two's file")
	}

	files, _, err := svc.List(ctxA, testID(10), 10, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 1 || files[0].ID != testID(1) {
		t.Fatalf("tenant one list = %+v, want only file %s", files, testID(1))
	}

	// The owning tenant can still operate on its own file.
	restored, err := svc.Restore(tenantCtx(testID(2)), testID(2), testID(20))
	if err != nil {
		t.Fatalf("owning tenant restore: %v", err)
	}
	if restored.DeletedAt != nil {
		t.Fatal("restored file must not be soft-deleted")
	}
	file, reader, err := svc.Download(tenantCtx(testID(2)), testID(2), testID(20))
	if err != nil {
		t.Fatalf("owning tenant download: %v", err)
	}
	_ = reader.Close()
	if file.TenantID != testID(2) {
		t.Fatalf("download tenant = %s, want %s", file.TenantID, testID(2))
	}
}

// tenantBlindRepo deliberately returns rows regardless of the request tenant,
// modelling a repository regression. The service must still refuse to expose
// another tenant's file.
type tenantBlindRepo struct {
	*mockFileRepo
}

func (r *tenantBlindRepo) GetByID(_ context.Context, id uuid.UUID) (*File, error) {
	f, ok := r.files[id]
	if !ok || f.DeletedAt != nil {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (r *tenantBlindRepo) GetByIDIncludingDeleted(_ context.Context, id uuid.UUID) (*File, error) {
	f, ok := r.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (r *tenantBlindRepo) Delete(_ context.Context, id uuid.UUID) (*File, error) {
	f, ok := r.files[id]
	if !ok || f.DeletedAt != nil {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = timePtr(time.Now())
	return f, nil
}

func (r *tenantBlindRepo) Restore(_ context.Context, id uuid.UUID) (*File, error) {
	f, ok := r.files[id]
	if !ok || f.DeletedAt == nil {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = nil
	return f, nil
}

func TestUploadService_RejectsLeakedCrossTenantRows(t *testing.T) {
	base := newMockFileRepo()
	repo := &tenantBlindRepo{mockFileRepo: base}
	store := newMockStorage()
	svc := NewUploadService(repo, newFakeTx(repo), store, testAuthorizer(), UploadOptions{})

	base.files[testID(1)] = &File{
		ID: testID(1), TenantID: testID(2), UserID: testID(20), OriginalName: "tenant-two.pdf",
		StoragePath: "2/20/b.pdf", MimeType: "application/pdf", Size: 20,
	}
	store.files["2/20/b.pdf"] = []byte("b")

	ctxA := tenantCtx(testID(1))
	_, err := svc.GetFileInfo(ctxA, testID(1), testID(20))
	requireNotFound(t, err)
	_, _, err = svc.Download(ctxA, testID(1), testID(20))
	requireNotFound(t, err)
	requireNotFound(t, svc.Delete(ctxA, testID(1), testID(20)))
	if base.files[testID(1)].DeletedAt != nil {
		t.Fatal("leaked cross-tenant delete must not mutate the row")
	}

	base.files[testID(1)].DeletedAt = timePtr(time.Now())
	_, err = svc.Restore(ctxA, testID(1), testID(20))
	requireNotFound(t, err)
	if base.files[testID(1)].DeletedAt == nil {
		t.Fatal("leaked cross-tenant restore must not revive the row")
	}
}
