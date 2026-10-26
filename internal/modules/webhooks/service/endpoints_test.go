package service

import (
	"context"
	"testing"
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

type rotateRecordingStore struct {
	EndpointStore
	expirePrevious bool
	calls          int
}

func (s *rotateRecordingStore) RotateEndpointSecret(_ context.Context, id int64, _ string, expirePrevious bool) (*Endpoint, error) {
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

	if _, _, err := svc.RotateEndpointSecret(context.Background(), 1, 9, false); err != nil {
		t.Fatalf("rotate default: %v", err)
	}
	if store.expirePrevious {
		t.Fatal("default rotation must keep the previous secret in grace")
	}
	if _, _, err := svc.RotateEndpointSecret(context.Background(), 1, 9, true); err != nil {
		t.Fatalf("rotate expirePrevious: %v", err)
	}
	if !store.expirePrevious {
		t.Fatal("expirePrevious must reach the repository")
	}
	if store.calls != 2 {
		t.Fatalf("rotate calls = %d, want 2", store.calls)
	}
}
