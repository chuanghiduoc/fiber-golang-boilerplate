package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Every mutation publishes its versioned domain event through the
// transaction, in order, for the file's tenant.
func TestMutationsPublishDomainEvents(t *testing.T) {
	repo := newMockFileRepo()
	tx := newFakeTx(repo)
	svc := NewUploadService(repo, tx, newMockStorage())
	admin := NewAdminFileService(repo, tx, newMockStorage())

	file, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := svc.Delete(systemCtx(), file.ID, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Restore(systemCtx(), file.ID, 1); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := admin.DeleteFile(systemCtx(), file.ID); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
	if _, err := admin.RestoreFile(systemCtx(), file.ID); err != nil {
		t.Fatalf("admin restore: %v", err)
	}

	want := []string{
		filescontracts.EventFileUploaded,
		filescontracts.EventFileDeleted,
		filescontracts.EventFileRestored,
		filescontracts.EventFileDeleted,
		filescontracts.EventFileRestored,
	}
	published := tx.events.events
	if len(published) != len(want) {
		t.Fatalf("published events = %d, want %d", len(published), len(want))
	}
	for i, eventType := range want {
		event := published[i]
		if event.Type != eventType || event.DataVersion != 1 || event.TenantID != 1 {
			t.Errorf("event %d = %s v%d tenant %d, want %s v1 tenant 1", i, event.Type, event.DataVersion, event.TenantID, eventType)
		}
		if event.ID == "" {
			t.Errorf("event %d has no id", i)
		}
	}
}

// The event data is the contracts payload, never the REST DTO: no tenant
// (it is in the envelope) and no download URL.
func TestEventPayloadsAreContracts(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	deleted := created.Add(time.Hour)
	file := &File{
		ID: 9, TenantID: 2, UserID: 3, OriginalName: "a.pdf", StoragePath: "2/3/x.pdf",
		MimeType: "application/pdf", Size: 42, CreatedAt: created,
	}
	deletedFile := *file
	deletedFile.DeletedAt = &deleted
	cases := []struct {
		name string
		data any
		want string
	}{
		{"uploaded", fileUploaded(file), `{"fileId":9,"ownerId":3,"originalName":"a.pdf","mimeType":"application/pdf","size":42,"createdAt":"2026-01-02T03:04:05Z"}`},
		{"restored", fileRestored(file), `{"fileId":9,"ownerId":3,"originalName":"a.pdf"}`},
		{"deleted", fileDeleted(&deletedFile), `{"fileId":9,"ownerId":3,"originalName":"a.pdf","deletedAt":"2026-01-02T04:04:05Z"}`},
	}

	for _, tc := range cases {
		data, err := json.Marshal(tc.data)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != tc.want {
			t.Errorf("%s payload =\n%s\nwant\n%s", tc.name, data, tc.want)
		}
	}
}

// A publish failure fails the mutation closed: upload removes the blob and
// returns 500, admin mutations return 500.
func TestPublishFailureFailsMutation(t *testing.T) {
	repo := newMockFileRepo()
	tx := newFakeTx(repo)
	tx.events.err = errors.New("queue unavailable")
	store := newMockStorage()
	svc := NewUploadService(repo, tx, store)

	_, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
	requireStatus(t, err, 500)
	if len(store.files) != 0 {
		t.Fatalf("stored files = %d, want the blob cleaned up", len(store.files))
	}

	repo.files[7] = &File{ID: 7, TenantID: 1, UserID: 1, StoragePath: "1/1/x.bin", CreatedAt: time.Now()}
	admin := NewAdminFileService(repo, tx, store)
	requireStatus(t, admin.DeleteFile(systemCtx(), 7), 500)
}

// A Tier 1 audit failure maps to 503 system.audit_unavailable.
func TestAdminMutationAuditFailureIsUnavailable(t *testing.T) {
	repo := newFakeAdminFileRepo()
	seedAdminFiles(repo, 1)
	tx := newFakeTx(repo)
	tx.audit = auditWriterFunc(func(context.Context, audit.Event) error { return errors.New("audit down") })
	svc := NewAdminFileService(repo, tx, newFakeAdminStorage())

	err := svc.DeleteFile(systemCtx(), 1)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Code != string(apperror.CodeSystemAuditUnavailable) {
		t.Fatalf("expected %s, got %v", apperror.CodeSystemAuditUnavailable, err)
	}
	if len(tx.events.events) != 0 {
		t.Fatalf("published events = %d, want none after an audit failure", len(tx.events.events))
	}
}
