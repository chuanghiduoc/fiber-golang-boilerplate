package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// TestEndpointAuditMetadataRedactsURLSecrets proves the append-only audit log
// records only the receiver's origin: endpoint URLs may carry tokens in the
// path or query (Slack hooks), which must never be written to audit metadata.
func TestEndpointAuditMetadataRedactsURLSecrets(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"slack-style path token", "https://hooks.slack.com/services/T000/B000/secret-token", "https://hooks.slack.com"},
		{"query token", "https://receiver.example/hook?token=super-secret&x=1", "https://receiver.example"},
		{"explicit port", "https://receiver.example:8443/hook/secret", "https://receiver.example:8443"},
		{"fragment", "http://receiver.example/hook#secret", "http://receiver.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			endpoint := &Endpoint{
				URL:         tc.url,
				EventTypes:  []string{"file.uploaded"},
				Status:      StatusActive,
				MaxAttempts: 8,
				TimeoutMS:   10000,
			}
			metadata := endpointAuditMetadata(endpoint)
			if got := metadata["url"]; got != tc.want {
				t.Fatalf("audit url = %v, want %q", got, tc.want)
			}
		})
	}
}

// endpointStatusStore captures the status a create draft carried.
type endpointStatusStore struct {
	EndpointStore
	status string
}

func (s *endpointStatusStore) CreateEndpoint(_ context.Context, draft CreateEndpointDraft) (*Endpoint, error) {
	s.status = draft.Status
	return &Endpoint{ID: testID(9), URL: draft.URL, EventTypes: draft.EventTypes, Status: draft.Status}, nil
}

func TestCreateEndpoint_StatusInput(t *testing.T) {
	codec, err := NewSecretCodec("endpoint-create-status-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	guard, err := webhook.NewGuard(nil)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	guard = guard.WithLookupIP(publicLookup)
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	for _, tc := range []struct {
		name   string
		status string
		want   string
	}{
		{"empty defaults to active", "", StatusActive},
		{"disabled is honoured", StatusDisabled, StatusDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &endpointStatusStore{}
			svc := New(store, nil, nil, codec, nil, Options{URLGuard: guard})
			if _, _, err := svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: "https://receiver.example/hook", Status: tc.status}); err != nil {
				t.Fatalf("CreateEndpoint: %v", err)
			}
			if store.status != tc.want {
				t.Fatalf("draft status = %q, want %q", store.status, tc.want)
			}
		})
	}

	store := &endpointStatusStore{}
	svc := New(store, nil, nil, codec, nil, Options{URLGuard: guard})
	_, _, err = svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: "https://receiver.example/hook", Status: "paused"})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("invalid status error = %v, want 400", err)
	}
}

type rotateRecordingStore struct {
	EndpointStore
	expirePrevious bool
	calls          int
}

func (s *rotateRecordingStore) RotateEndpointSecret(_ context.Context, id uuid.UUID, _ string, expirePrevious bool) (*Endpoint, error) {
	s.calls++
	s.expirePrevious = expirePrevious
	return &Endpoint{ID: id}, nil
}

func TestRotateEndpointSecret_ExpirePreviousOption(t *testing.T) {
	codec, err := NewSecretCodec("rotate-expire-previous-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	store := &rotateRecordingStore{}
	svc := New(store, nil, nil, codec, nil, Options{})

	if _, _, err := svc.RotateEndpointSecret(tenant.WithID(context.Background(), tenant.DefaultID), testID(1), testID(9), false); err != nil {
		t.Fatalf("rotate default: %v", err)
	}
	if store.expirePrevious {
		t.Fatal("default rotation must keep the previous secret in grace")
	}
	if _, _, err := svc.RotateEndpointSecret(tenant.WithID(context.Background(), tenant.DefaultID), testID(1), testID(9), true); err != nil {
		t.Fatalf("rotate expirePrevious: %v", err)
	}
	if !store.expirePrevious {
		t.Fatal("expirePrevious must reach the repository")
	}
	if store.calls != 2 {
		t.Fatalf("rotate calls = %d, want 2", store.calls)
	}
}
