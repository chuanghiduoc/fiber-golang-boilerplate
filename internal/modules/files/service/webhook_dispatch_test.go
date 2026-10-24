package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
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
	svc := newTestUploadService(repo, store)
	dispatcher := &recordingWebhookDispatcher{}
	AttachWebhookDispatcher(svc, dispatcher)

	resp, err := svc.Upload(context.Background(), 1, "photo.jpg", strings.NewReader("image-data"), 10, "image/jpeg")
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

	if err := svc.Delete(context.Background(), resp.ID, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(dispatcher.events) != 2 {
		t.Fatalf("events = %d, want 2", len(dispatcher.events))
	}
	if dispatcher.events[1].Type != webhookcontracts.EventFileDeleted {
		t.Errorf("event type = %q, want %q", dispatcher.events[1].Type, webhookcontracts.EventFileDeleted)
	}
}

func TestUploadWebhookFailureDoesNotFailMutation(t *testing.T) {
	repo := newMockFileRepo()
	store := newMockStorage()
	svc := newTestUploadService(repo, store)
	AttachWebhookDispatcher(svc, &recordingWebhookDispatcher{err: errors.New("dispatch unavailable")})

	if _, err := svc.Upload(context.Background(), 1, "photo.jpg", strings.NewReader("data"), 4, "image/jpeg"); err != nil {
		t.Fatalf("upload must succeed when webhook dispatch fails: %v", err)
	}
	if len(store.files) != 1 {
		t.Fatalf("stored files = %d, want 1", len(store.files))
	}
}
