package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// inboundSourceRecordingStore captures the create draft and the rotation
// options so the service contract can be asserted without a database.
type inboundSourceRecordingStore struct {
	InboundStore
	created        InboundSourceDraft
	expirePrevious bool
}

func (s *inboundSourceRecordingStore) CreateInboundSource(_ context.Context, draft InboundSourceDraft) (*InboundSource, error) {
	s.created = draft
	return &InboundSource{ID: testID(4), Name: draft.Name, Slug: draft.Slug, Status: draft.Status}, nil
}

func (s *inboundSourceRecordingStore) RotateInboundSourceSecret(_ context.Context, id uuid.UUID, _ string, expirePrevious bool) (*InboundSource, error) {
	s.expirePrevious = expirePrevious
	return &InboundSource{ID: id}, nil
}

func TestCreateInboundSource_StatusInput(t *testing.T) {
	codec, err := NewSecretCodec("inbound-create-status-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
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
			store := &inboundSourceRecordingStore{}
			svc := New(nil, nil, store, codec, nil, Options{})
			if _, _, err := svc.CreateInboundSource(ctx, testID(1), CreateInboundSourceInput{Name: "Stripe", Slug: "stripe", Status: tc.status}); err != nil {
				t.Fatalf("CreateInboundSource: %v", err)
			}
			if store.created.Status != tc.want {
				t.Fatalf("draft status = %q, want %q", store.created.Status, tc.want)
			}
		})
	}

	store := &inboundSourceRecordingStore{}
	svc := New(nil, nil, store, codec, nil, Options{})
	_, _, err = svc.CreateInboundSource(ctx, testID(1), CreateInboundSourceInput{Name: "Stripe", Slug: "stripe", Status: "paused"})
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || resolvedStatus(appErr) != 400 {
		t.Fatalf("invalid status error = %v, want 400", err)
	}
}

func TestRotateInboundSourceSecret_ExpirePreviousOption(t *testing.T) {
	codec, err := NewSecretCodec("inbound-rotate-expire-previous-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	store := &inboundSourceRecordingStore{}
	svc := New(nil, nil, store, codec, nil, Options{})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	if _, _, err := svc.RotateInboundSourceSecret(ctx, testID(1), testID(4), false); err != nil {
		t.Fatalf("rotate default: %v", err)
	}
	if store.expirePrevious {
		t.Fatal("default rotation must keep the previous secret in grace")
	}
	if _, _, err := svc.RotateInboundSourceSecret(ctx, testID(1), testID(4), true); err != nil {
		t.Fatalf("rotate expirePrevious: %v", err)
	}
	if !store.expirePrevious {
		t.Fatal("expirePrevious must reach the repository")
	}
}

// failingEndpointStore is a repository that fails every list call.
type failingEndpointStore struct {
	EndpointStore
	err error
}

func (s *failingEndpointStore) ListEndpoints(context.Context, pagination.DBCursor) ([]Endpoint, error) {
	return nil, s.err
}

func TestServiceWrapsStoreCause(t *testing.T) {
	cause := errors.New("connection reset")
	svc := New(&failingEndpointStore{err: cause}, nil, nil, nil, nil, Options{})

	_, _, err := svc.ListEndpoints(tenant.WithID(context.Background(), tenant.DefaultID), 10, "")
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("ListEndpoints error = %v, want *apperror.AppError", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("ListEndpoints error does not preserve its cause: %v", err)
	}
	if appErr.Code != string(apperror.CodeSystemInternal) {
		t.Fatalf("code = %q, want %q", appErr.Code, apperror.CodeSystemInternal)
	}
}
