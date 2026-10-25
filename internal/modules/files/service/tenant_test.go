package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func tenantCtx(id int64) context.Context {
	return authz.SystemContext(tenant.WithID(context.Background(), id))
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != 404 {
		t.Fatalf("error = %v, want 404 not found", err)
	}
}

func TestUpload_StoragePathIsTenantScoped(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := newTestUploadService(repo, store)

	resp, err := svc.Upload(tenantCtx(2), 10, "photo.jpg", strings.NewReader("pdf-data"), 8, "application/pdf")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if resp.TenantID != 2 {
		t.Fatalf("response tenant = %d, want 2", resp.TenantID)
	}
	if len(store.files) != 1 {
		t.Fatalf("stored blobs = %d, want 1", len(store.files))
	}
	for path := range store.files {
		if !strings.HasPrefix(path, "2/10/") {
			t.Fatalf("storage path = %q, want tenant/user prefix 2/10/", path)
		}
		if !strings.HasSuffix(path, ".pdf") {
			t.Fatalf("storage path = %q, want .pdf extension", path)
		}
	}
	if repo.files[resp.ID].TenantID != 2 {
		t.Fatalf("repo tenant = %d, want 2", repo.files[resp.ID].TenantID)
	}

	// The same user id in another tenant gets a different key space.
	other, err := svc.Upload(tenantCtx(3), 10, "photo.jpg", strings.NewReader("pdf-data"), 8, "application/pdf")
	if err != nil {
		t.Fatalf("upload tenant 3: %v", err)
	}
	if other.TenantID != 3 {
		t.Fatalf("response tenant = %d, want 3", other.TenantID)
	}
	for path := range store.files {
		if strings.HasPrefix(path, "3/10/") {
			return
		}
	}
	t.Fatal("tenant 3 upload did not use the 3/10/ key space")
}

func TestUpload_RejectsInvalidTenant(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := newTestUploadService(repo, store)

	_, err := svc.Upload(tenantCtx(-1), 10, "photo.jpg", strings.NewReader("data"), 4, "application/pdf")
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

	repo.files[1] = &filesqlc.File{
		ID: 1, TenantID: 1, UserID: 10, OriginalName: "tenant-one.pdf",
		StoragePath: "1/10/a.pdf", MimeType: "application/pdf", Size: 10,
	}
	repo.files[2] = &filesqlc.File{
		ID: 2, TenantID: 2, UserID: 20, OriginalName: "tenant-two.pdf",
		StoragePath: "2/20/b.pdf", MimeType: "application/pdf", Size: 20,
	}
	repo.files[3] = &filesqlc.File{
		ID: 3, TenantID: 2, UserID: 10, OriginalName: "same-user-other-tenant.pdf",
		StoragePath: "2/10/c.pdf", MimeType: "application/pdf", Size: 30,
	}
	repo.nextID = 4
	store.files["1/10/a.pdf"] = []byte("a")
	store.files["2/20/b.pdf"] = []byte("b")
	store.files["2/10/c.pdf"] = []byte("c")

	ctxA := tenantCtx(1)

	_, err := svc.GetFileInfo(ctxA, 2, 20)
	requireNotFound(t, err)
	_, err = svc.GetFileInfo(ctxA, 3, 10)
	requireNotFound(t, err)
	_, _, err = svc.Download(ctxA, 2, 20)
	requireNotFound(t, err)
	requireNotFound(t, svc.Delete(ctxA, 2, 20))
	if repo.files[2].DeletedAt.Valid {
		t.Fatal("cross-tenant delete must not touch tenant two's file")
	}

	repo.files[2].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	_, err = svc.Restore(ctxA, 2, 20)
	requireNotFound(t, err)
	if !repo.files[2].DeletedAt.Valid {
		t.Fatal("cross-tenant restore must not revive tenant two's file")
	}

	files, _, err := svc.List(ctxA, 10, 10, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) != 1 || files[0].ID != 1 {
		t.Fatalf("tenant one list = %+v, want only file 1", files)
	}

	// The owning tenant can still operate on its own file.
	restored, err := svc.Restore(tenantCtx(2), 2, 20)
	if err != nil {
		t.Fatalf("owning tenant restore: %v", err)
	}
	if restored.DeletedAt != nil {
		t.Fatal("restored file must not be soft-deleted")
	}
	file, reader, err := svc.Download(tenantCtx(2), 2, 20)
	if err != nil {
		t.Fatalf("owning tenant download: %v", err)
	}
	_ = reader.Close()
	if file.TenantID != 2 {
		t.Fatalf("download tenant = %d, want 2", file.TenantID)
	}
}

// tenantBlindRepo deliberately returns rows regardless of the request tenant,
// modelling a repository regression. The service must still refuse to expose
// another tenant's file.
type tenantBlindRepo struct {
	*mockFileRepo
}

func (r *tenantBlindRepo) GetByID(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := r.files[id]
	if !ok || f.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (r *tenantBlindRepo) GetByIDIncludingDeleted(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := r.files[id]
	if !ok {
		return nil, apperror.ErrNotFound
	}
	return f, nil
}

func (r *tenantBlindRepo) Delete(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := r.files[id]
	if !ok || f.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return f, nil
}

func (r *tenantBlindRepo) Restore(_ context.Context, id int64) (*filesqlc.File, error) {
	f, ok := r.files[id]
	if !ok || !f.DeletedAt.Valid {
		return nil, apperror.ErrNotFound
	}
	f.DeletedAt = pgtype.Timestamptz{}
	return f, nil
}

func TestUploadService_RejectsLeakedCrossTenantRows(t *testing.T) {
	base := newMockFileRepo()
	repo := &tenantBlindRepo{mockFileRepo: base}
	store := newMockStorage()
	svc := NewUploadService(repo, store)

	base.files[1] = &filesqlc.File{
		ID: 1, TenantID: 2, UserID: 20, OriginalName: "tenant-two.pdf",
		StoragePath: "2/20/b.pdf", MimeType: "application/pdf", Size: 20,
	}
	store.files["2/20/b.pdf"] = []byte("b")

	ctxA := tenantCtx(1)
	_, err := svc.GetFileInfo(ctxA, 1, 20)
	requireNotFound(t, err)
	_, _, err = svc.Download(ctxA, 1, 20)
	requireNotFound(t, err)
	requireNotFound(t, svc.Delete(ctxA, 1, 20))
	if base.files[1].DeletedAt.Valid {
		t.Fatal("leaked cross-tenant delete must not mutate the row")
	}

	base.files[1].DeletedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	_, err = svc.Restore(ctxA, 1, 20)
	requireNotFound(t, err)
	if !base.files[1].DeletedAt.Valid {
		t.Fatal("leaked cross-tenant restore must not revive the row")
	}
}
