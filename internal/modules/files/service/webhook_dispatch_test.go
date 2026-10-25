package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type recordingWebhookDispatcher struct {
	events []webhookcontracts.Event
	err    error
}

func (r *recordingWebhookDispatcher) Dispatch(_ context.Context, events ...webhookcontracts.Event) (int, error) {
	r.events = append(r.events, events...)
	return len(events), r.err
}

func TestUploadDispatchesWebhookEvent(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	dispatcher := &recordingWebhookDispatcher{}
	svc := NewUploadService(repo, newFakeTx(repo), store, dispatcher)

	resp, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("image-data"), 10, "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(dispatcher.events) != 1 {
		t.Fatalf("events = %d, want 1", len(dispatcher.events))
	}
	event := dispatcher.events[0]
	if event.Type != webhookcontracts.EventFileUploaded {
		t.Errorf("event type = %q, want %q", event.Type, webhookcontracts.EventFileUploaded)
	}
	if event.TenantID != 1 {
		t.Errorf("tenant = %d, want 1", event.TenantID)
	}
	if !strings.Contains(string(event.Data), `"id":1`) {
		t.Errorf("payload = %s, want file id", event.Data)
	}

	if err := svc.Delete(systemCtx(), resp.ID, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(dispatcher.events) != 2 {
		t.Fatalf("events = %d, want 2", len(dispatcher.events))
	}
	if dispatcher.events[1].Type != webhookcontracts.EventFileDeleted {
		t.Errorf("event type = %q, want %q", dispatcher.events[1].Type, webhookcontracts.EventFileDeleted)
	}
}

func TestUserRestoreDispatchesWebhookEvent(t *testing.T) {
	repo := newMockFileRepo()
	dispatcher := &recordingWebhookDispatcher{}
	svc := NewUploadService(repo, newFakeTx(repo), newMockStorage(), dispatcher)

	resp, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("image-data"), 10, "image/jpeg")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := svc.Delete(systemCtx(), resp.ID, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Restore(systemCtx(), resp.ID, 1); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(dispatcher.events) != 3 {
		t.Fatalf("events = %d, want 3", len(dispatcher.events))
	}
	event := dispatcher.events[2]
	if event.Type != webhookcontracts.EventFileRestored || event.TenantID != 1 {
		t.Fatalf("restore event = %+v, want %q for tenant 1", event, webhookcontracts.EventFileRestored)
	}
	if !strings.Contains(string(event.Data), `"id":1`) {
		t.Errorf("payload = %s, want file id", event.Data)
	}
}

func TestAdminDeleteAndRestoreDispatchWebhookEvents(t *testing.T) {
	repo := newFakeAdminFileRepo()
	dispatcher := &recordingWebhookDispatcher{}
	svc := NewAdminFileService(repo, newFakeTx(repo), newFakeAdminStorage(), dispatcher)
	seedAdminFiles(repo, 1)

	if err := svc.DeleteFile(systemCtx(), 1); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
	if _, err := svc.RestoreFile(systemCtx(), 1); err != nil {
		t.Fatalf("admin restore: %v", err)
	}
	if len(dispatcher.events) != 2 {
		t.Fatalf("events = %d, want 2", len(dispatcher.events))
	}
	for i, want := range []string{webhookcontracts.EventFileDeleted, webhookcontracts.EventFileRestored} {
		event := dispatcher.events[i]
		if event.Type != want || event.TenantID != 1 {
			t.Errorf("event %d = %+v, want %q for tenant 1", i, event, want)
		}
		if !strings.Contains(string(event.Data), `"id":1`) {
			t.Errorf("event %d payload = %s, want file id", i, event.Data)
		}
	}
}

func TestUploadWebhookFailureDoesNotFailMutation(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := NewUploadService(repo, newFakeTx(repo), store, &recordingWebhookDispatcher{err: errors.New("dispatch unavailable")})

	if _, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg"); err != nil {
		t.Fatalf("upload must succeed when webhook dispatch fails: %v", err)
	}
	if len(store.files) != 1 {
		t.Fatalf("stored files = %d, want 1", len(store.files))
	}
}

// With a transactional outbox the event is enqueued inside the transaction
// and never dispatched a second time after commit.
func TestMutationsUseTransactionalOutbox(t *testing.T) {
	repo := newMockFileRepo()
	tx := newFakeTx(repo)
	outbox := &recordingOutbox{}
	tx.outbox = outbox
	afterCommit := &recordingWebhookDispatcher{}
	svc := NewUploadService(repo, tx, newMockStorage(), afterCommit)
	admin := NewAdminFileService(repo, tx, newMockStorage(), afterCommit)

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
		webhookcontracts.EventFileUploaded,
		webhookcontracts.EventFileDeleted,
		webhookcontracts.EventFileRestored,
		webhookcontracts.EventFileDeleted,
		webhookcontracts.EventFileRestored,
	}
	if len(outbox.events) != len(want) {
		t.Fatalf("outbox events = %d, want %d", len(outbox.events), len(want))
	}
	for i, eventType := range want {
		if outbox.events[i].Type != eventType {
			t.Errorf("outbox event %d = %q, want %q", i, outbox.events[i].Type, eventType)
		}
	}
	if len(afterCommit.events) != 0 {
		t.Fatalf("post-commit events = %d, want 0 when the outbox is wired", len(afterCommit.events))
	}
}

// An outbox failure fails the mutation closed: upload removes the blob and
// returns 500, admin mutations return 500.
func TestOutboxFailureFailsMutation(t *testing.T) {
	repo := newMockFileRepo()
	tx := newFakeTx(repo)
	tx.outbox = &recordingOutbox{err: errors.New("queue unavailable")}
	store := newMockStorage()
	svc := NewUploadService(repo, tx, store, nil)

	_, err := svc.Upload(systemCtx(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg")
	requireStatus(t, err, 500)
	if len(store.files) != 0 {
		t.Fatalf("stored files = %d, want the blob cleaned up", len(store.files))
	}

	repo.files[7] = &File{ID: 7, TenantID: 1, UserID: 1, StoragePath: "1/1/x.bin", CreatedAt: time.Now()}
	admin := NewAdminFileService(repo, tx, store, nil)
	requireStatus(t, admin.DeleteFile(systemCtx(), 7), 500)
}

// A Tier 1 audit failure maps to 503 system.audit_unavailable.
func TestAdminMutationAuditFailureIsUnavailable(t *testing.T) {
	repo := newFakeAdminFileRepo()
	seedAdminFiles(repo, 1)
	tx := newFakeTx(repo)
	tx.audit = auditWriterFunc(func(context.Context, audit.Event) error { return errors.New("audit down") })
	svc := NewAdminFileService(repo, tx, newFakeAdminStorage(), nil)

	err := svc.DeleteFile(systemCtx(), 1)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Code != string(apperror.CodeSystemAuditUnavailable) {
		t.Fatalf("expected %s, got %v", apperror.CodeSystemAuditUnavailable, err)
	}
}

// The file.uploaded payload must stay byte-identical to the former
// dto.FileResponse encoding until the versioned envelope (ADR 0011) lands.
func TestUploadedEventPayloadIsStable(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	data, err := json.Marshal(uploadedEventData(&File{
		ID: 9, TenantID: 2, UserID: 3, OriginalName: "a.pdf", StoragePath: "2/3/x.pdf",
		MimeType: "application/pdf", Size: 42, CreatedAt: created,
	}))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":9,"tenantId":2,"userId":3,"originalName":"a.pdf","mimeType":"application/pdf","size":42,"url":"/api/v1/files/9/download","createdAt":"2026-01-02T03:04:05Z"}`
	if string(data) != want {
		t.Fatalf("payload =\n%s\nwant\n%s", data, want)
	}
}
