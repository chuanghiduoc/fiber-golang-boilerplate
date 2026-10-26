package service

import (
	"context"
	"errors"
	"testing"

	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

type recordingEndpointStore struct {
	EndpointStore
	created int
	updated int
}

func (s *recordingEndpointStore) CreateEndpoint(_ context.Context, draft CreateEndpointDraft) (*Endpoint, error) {
	s.created++
	if draft.SealSecret == nil {
		return nil, errors.New("missing secret sealer")
	}
	ciphertext, err := draft.SealSecret(9)
	if err != nil {
		return nil, err
	}
	if ciphertext == "" {
		return nil, errors.New("empty secret ciphertext")
	}
	return &Endpoint{ID: 9, URL: draft.URL, EventTypes: draft.EventTypes, Status: draft.Status}, nil
}

func (s *recordingEndpointStore) UpdateEndpoint(_ context.Context, draft UpdateEndpointDraft) (*Endpoint, error) {
	s.updated++
	return &Endpoint{ID: draft.ID, URL: draft.URL, EventTypes: draft.EventTypes, Status: draft.Status}, nil
}

func TestService_EndpointURLPortPolicy(t *testing.T) {
	codec, err := NewSecretCodec("port-policy-test-encryption-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	guard, err := webhook.NewGuardWithPorts(nil, []int{8443})
	if err != nil {
		t.Fatalf("NewGuardWithPorts: %v", err)
	}
	store := &recordingEndpointStore{}
	svc := New(store, nil, nil, codec, nil, Options{URLGuard: guard})
	ctx := context.Background()

	for _, raw := range []string{
		"https://receiver.example:6379/hook",
		"http://receiver.example:6379/hook",
	} {
		_, _, err := svc.CreateEndpoint(ctx, 1, CreateEndpointInput{URL: raw})
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || appErr.Status != 400 {
			t.Fatalf("CreateEndpoint(%q) error = %v, want 400", raw, err)
		}
		if appErr.Code != string(webhookscatalog.CodeWebhookPortNotAllowed) {
			t.Fatalf("CreateEndpoint(%q) code = %q, want %q", raw, appErr.Code, webhookscatalog.CodeWebhookPortNotAllowed)
		}
	}
	if store.created != 0 {
		t.Fatalf("rejected creates reached the store %d times", store.created)
	}

	// Implicit ports use the scheme default (443/80) and are allowed, as is
	// the configured extension port.
	for _, raw := range []string{"https://receiver.example/hook", "http://receiver.example/hook", "https://receiver.example:8443/hook"} {
		if _, _, err := svc.CreateEndpoint(ctx, 1, CreateEndpointInput{URL: raw}); err != nil {
			t.Fatalf("CreateEndpoint(%q) rejected: %v", raw, err)
		}
	}
	if store.created != 3 {
		t.Fatalf("created = %d, want 3", store.created)
	}

	_, err = svc.UpdateEndpoint(ctx, 1, 9, UpdateEndpointInput{URL: "https://receiver.example:6379/hook", EventTypes: []string{"*"}, Status: StatusActive})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Code != string(webhookscatalog.CodeWebhookPortNotAllowed) {
		t.Fatalf("UpdateEndpoint port error = %v, want %q", err, webhookscatalog.CodeWebhookPortNotAllowed)
	}
	if store.updated != 0 {
		t.Fatal("a rejected update reached the store")
	}
}
