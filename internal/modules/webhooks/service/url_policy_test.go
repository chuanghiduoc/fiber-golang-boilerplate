package service

import (
	"context"
	"errors"
	"net"
	"testing"

	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// publicLookup resolves every test hostname to a public address unless the
// caller installs a different mock.
func publicLookup(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

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
	ciphertext, err := draft.SealSecret(testID(9))
	if err != nil {
		return nil, err
	}
	if ciphertext == "" {
		return nil, errors.New("empty secret ciphertext")
	}
	return &Endpoint{ID: testID(9), URL: draft.URL, EventTypes: draft.EventTypes, Status: draft.Status}, nil
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
	guard = guard.WithLookupIP(publicLookup)
	store := &recordingEndpointStore{}
	svc := New(store, nil, nil, codec, nil, Options{URLGuard: guard})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	for _, raw := range []string{
		"https://receiver.example:6379/hook",
		"http://receiver.example:6379/hook",
	} {
		_, _, err := svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: raw})
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
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
		if _, _, err := svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: raw}); err != nil {
			t.Fatalf("CreateEndpoint(%q) rejected: %v", raw, err)
		}
	}
	if store.created != 3 {
		t.Fatalf("created = %d, want 3", store.created)
	}

	_, err = svc.UpdateEndpoint(ctx, testID(1), testID(9), UpdateEndpointInput{URL: "https://receiver.example:6379/hook", EventTypes: []string{"*"}, Status: StatusActive})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Code != string(webhookscatalog.CodeWebhookPortNotAllowed) {
		t.Fatalf("UpdateEndpoint port error = %v, want %q", err, webhookscatalog.CodeWebhookPortNotAllowed)
	}
	if store.updated != 0 {
		t.Fatal("a rejected update reached the store")
	}
}

// TestService_EndpointURLDNSValidated proves create/update resolve the hostname
// before accepting it, so an internal address is refused at configuration time
// instead of failing every delivery later.
func TestService_EndpointURLDNSValidated(t *testing.T) {
	codec, err := NewSecretCodec("dns-policy-test-encryption-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	guard, err := webhook.NewGuard(nil)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	guard = guard.WithLookupIP(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "internal.example" {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil
		}
		return publicLookup(tenant.WithID(context.Background(), tenant.DefaultID), host)
	})
	store := &recordingEndpointStore{}
	svc := New(store, nil, nil, codec, nil, Options{URLGuard: guard})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	_, _, err = svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: "https://internal.example/hook"})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("internal target error = %v, want 400", err)
	}
	if store.created != 0 {
		t.Fatal("a blocked target must not reach the store")
	}
	_, err = svc.UpdateEndpoint(ctx, testID(1), testID(9), UpdateEndpointInput{
		URL: "https://internal.example/hook", EventTypes: []string{"*"}, Status: StatusActive,
	})
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("internal update error = %v, want 400", err)
	}
	if store.updated != 0 {
		t.Fatal("a blocked update must not reach the store")
	}

	if _, _, err := svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: "https://receiver.example/hook"}); err != nil {
		t.Fatalf("public target rejected: %v", err)
	}
}

func TestService_RequireHTTPSSetting(t *testing.T) {
	codec, err := NewSecretCodec("https-policy-test-encryption-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	guard, err := webhook.NewGuard(nil)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	guard = guard.WithLookupIP(publicLookup)
	store := &recordingEndpointStore{}
	svc := New(store, nil, nil, codec, nil, Options{URLGuard: guard, RequireHTTPS: true})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	_, _, err = svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: "http://receiver.example/hook"})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("http target error = %v, want 400", err)
	}
	if _, _, err := svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: "https://receiver.example/hook"}); err != nil {
		t.Fatalf("https target rejected: %v", err)
	}

	// The default keeps http available outside production.
	store = &recordingEndpointStore{}
	svc = New(store, nil, nil, codec, nil, Options{URLGuard: guard})
	if _, _, err := svc.CreateEndpoint(ctx, testID(1), CreateEndpointInput{URL: "http://receiver.example/hook"}); err != nil {
		t.Fatalf("default policy rejected http: %v", err)
	}
}
