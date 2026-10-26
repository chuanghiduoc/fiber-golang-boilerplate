package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

type fakeInboundSourceStore struct {
	InboundStore
	source *InboundSource
	err    error
}

func (f *fakeInboundSourceStore) GetInboundSourceByID(context.Context, int64) (*InboundSource, error) {
	if f.err != nil {
		return nil, f.err
	}
	clone := *f.source
	return &clone, nil
}

func inboundTestHeaders(t *testing.T, secret, eventID string, body []byte, now time.Time) http.Header {
	t.Helper()
	key, err := webhook.DecodeSecret(secret)
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	headers := http.Header{}
	headers.Set(webhook.HeaderID, eventID)
	headers.Set(webhook.HeaderTimestamp, strconv.FormatInt(now.Unix(), 10))
	headers.Set(webhook.HeaderSignature, webhook.Sign(key, eventID, now, body))
	return headers
}

type countingTenantChecker struct {
	calls  int
	active bool
	err    error
}

func (c *countingTenantChecker) TenantActive(context.Context, int64) (bool, error) {
	c.calls++
	return c.active, c.err
}

// TestReceiveInbound_UnknownSourceRunsDummyTenantCheck proves the unknown-id
// branch does the same tenant lookup as an existing source, so response
// latency does not reveal which sequential source ids exist.
func TestReceiveInbound_UnknownSourceRunsDummyTenantCheck(t *testing.T) {
	codec, err := NewSecretCodec("inbound-timing-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	checker := &countingTenantChecker{}
	store := &fakeInboundSourceStore{err: apperror.ErrNotFound}
	svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute, Tenants: checker})
	body := []byte(`{"type":"invoice.paid"}`)
	headers := inboundTestHeaders(t, testInboundSecret(t), "evt_1", body, time.Now().UTC())

	_, err = svc.ReceiveInbound(context.Background(), 42, headers, body)
	if !errors.Is(err, errInboundSourceUnavailable) && !isUnauthorized(err) {
		t.Fatalf("error = %v, want the generic 401", err)
	}
	if checker.calls != 1 {
		t.Fatalf("tenant checks = %d, want 1 dummy check for an unknown source", checker.calls)
	}
}

// TestReceiveInbound_DisabledSourceRunsDummyTenantCheck proves a disabled
// source still spends the tenant lookup the real path skipped, so its latency
// matches an active source.
func TestReceiveInbound_DisabledSourceRunsDummyTenantCheck(t *testing.T) {
	codec, err := NewSecretCodec("inbound-disabled-timing-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	checker := &countingTenantChecker{}
	store := &fakeInboundSourceStore{source: &InboundSource{ID: 42, TenantID: 7, Status: StatusDisabled}}
	svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute, Tenants: checker})
	body := []byte(`{"type":"invoice.paid"}`)
	headers := inboundTestHeaders(t, testInboundSecret(t), "evt_1", body, time.Now().UTC())

	_, err = svc.ReceiveInbound(context.Background(), 42, headers, body)
	if !isUnauthorized(err) {
		t.Fatalf("error = %v, want the generic 401", err)
	}
	if checker.calls != 1 {
		t.Fatalf("tenant checks = %d, want 1 dummy check for a disabled source", checker.calls)
	}
}

// TestReceiveInbound_TenantCheckFailureLooksLikeBadSignature proves a database
// error while resolving the source tenant is logged but still returns 401, not
// a 500 that distinguishes the source id.
func TestReceiveInbound_TenantCheckFailureLooksLikeBadSignature(t *testing.T) {
	codec, err := NewSecretCodec("inbound-tenant-error-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	secret := testInboundSecret(t)
	ciphertext, err := codec.Seal(InboundSourceSecretBinding(7, 42), []byte(secret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	source := &InboundSource{ID: 42, TenantID: 7, Status: StatusActive, SecretCiphertext: ciphertext}
	checker := &countingTenantChecker{err: errors.New("database is unavailable")}
	store := &fakeInboundSourceStore{source: source}
	svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute, Tenants: checker})
	body := []byte(`{"type":"invoice.paid"}`)
	headers := inboundTestHeaders(t, secret, "evt_1", body, time.Now().UTC())

	_, err = svc.ReceiveInbound(context.Background(), 42, headers, body)
	if !isUnauthorized(err) {
		t.Fatalf("error = %v, want the generic 401", err)
	}
	if checker.calls != 1 {
		t.Fatalf("tenant checks = %d, want 1", checker.calls)
	}
}

func testInboundSecret(t *testing.T) string {
	t.Helper()
	secret, err := webhook.NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	return secret
}

func isUnauthorized(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && appErr.Status == http.StatusUnauthorized
}

func TestReceiveInbound_UnavailableSourceLooksLikeBadSignature(t *testing.T) {
	codec, err := NewSecretCodec("inbound-enumeration-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	secret, err := webhook.NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	ciphertext, err := codec.Seal(InboundSourceSecretBinding(7, 42), []byte(secret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	body := []byte(`{"type":"invoice.paid"}`)
	now := time.Now().UTC()

	active := &InboundSource{ID: 42, TenantID: 7, Status: StatusActive, SecretCiphertext: ciphertext}
	disabled := *active
	disabled.Status = StatusDisabled

	cases := map[string]*fakeInboundSourceStore{
		"unknown source":   {err: apperror.ErrNotFound},
		"disabled source":  {source: &disabled},
		"deleted source":   {err: apperror.ErrNotFound},
		"existing but bad": {source: active},
	}
	var problems []*apperror.AppError
	for name, store := range cases {
		t.Run(name, func(t *testing.T) {
			svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute})
			headers := inboundTestHeaders(t, secret, "evt_1", body, now)
			if name == "existing but bad" {
				headers.Set(webhook.HeaderSignature, "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
			}
			_, err := svc.ReceiveInbound(context.Background(), 42, headers, body)
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("error = %v, want an AppError", err)
			}
			if appErr.Status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", appErr.Status)
			}
			problems = append(problems, appErr)
		})
	}

	if len(problems) != len(cases) {
		t.Fatalf("collected %d problems, want %d", len(problems), len(cases))
	}
	first := problems[0]
	for _, problem := range problems[1:] {
		if problem.Status != first.Status || problem.Code != first.Code || problem.Detail != first.Detail {
			t.Fatalf("problem mismatch: %+v vs %+v", problem, first)
		}
	}
	if first.Detail != "webhook signature is invalid" {
		t.Fatalf("detail = %q, want the generic signature message", first.Detail)
	}
}
