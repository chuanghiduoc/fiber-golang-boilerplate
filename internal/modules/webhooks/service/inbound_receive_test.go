package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

type fakeInboundSourceStore struct {
	InboundStore
	source  *InboundSource
	secrets *SigningSecrets
	err     error
}

func (f *fakeInboundSourceStore) GetInboundSourceByID(context.Context, uuid.UUID) (*InboundSource, error) {
	if f.err != nil {
		return nil, f.err
	}
	clone := *f.source
	return &clone, nil
}

func (f *fakeInboundSourceStore) InboundSigningSecrets(context.Context, uuid.UUID) (*SigningSecrets, error) {
	if f.secrets == nil {
		return nil, apperror.ErrNotFound
	}
	return f.secrets, nil
}

func (f *fakeInboundSourceStore) CreateInboundEvent(context.Context, InboundEventDraft) (uuid.UUID, bool, error) {
	return testID(1), true, nil
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

func (c *countingTenantChecker) TenantActive(context.Context, uuid.UUID) (bool, error) {
	c.calls++
	return c.active, c.err
}

// TestReceiveInbound_UnknownSourceRunsDummyTenantCheck proves the unknown-id
// branch does the same tenant lookup as an existing source, so response
// latency does not reveal which source ids exist.
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

	_, err = svc.ReceiveInbound(context.Background(), testID(42), headers, body)
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
	store := &fakeInboundSourceStore{source: &InboundSource{ID: testID(42), TenantID: testID(7), Status: StatusDisabled}}
	svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute, Tenants: checker})
	body := []byte(`{"type":"invoice.paid"}`)
	headers := inboundTestHeaders(t, testInboundSecret(t), "evt_1", body, time.Now().UTC())

	_, err = svc.ReceiveInbound(context.Background(), testID(42), headers, body)
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
	ciphertext, err := codec.Seal(InboundSourceSecretBinding(testID(7), testID(42)), []byte(secret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	source := &InboundSource{ID: testID(42), TenantID: testID(7), Status: StatusActive}
	checker := &countingTenantChecker{err: errors.New("database is unavailable")}
	store := &fakeInboundSourceStore{source: source, secrets: &SigningSecrets{CurrentCiphertext: ciphertext}}
	svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute, Tenants: checker})
	body := []byte(`{"type":"invoice.paid"}`)
	headers := inboundTestHeaders(t, secret, "evt_1", body, time.Now().UTC())

	_, err = svc.ReceiveInbound(context.Background(), testID(42), headers, body)
	if !isUnauthorized(err) {
		t.Fatalf("error = %v, want the generic 401", err)
	}
	if checker.calls != 1 {
		t.Fatalf("tenant checks = %d, want 1", checker.calls)
	}
}

// TestReceiveInbound_AcceptsPreviousSecretDuringGrace proves verification uses
// the versioned secret rows: a request signed with the previous secret still
// verifies while the rotation grace window is open.
func TestReceiveInbound_AcceptsPreviousSecretDuringGrace(t *testing.T) {
	codec, err := NewSecretCodec("inbound-previous-secret-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	currentSecret := testInboundSecret(t)
	previousSecret := testInboundSecret(t)
	binding := InboundSourceSecretBinding(testID(7), testID(42))
	currentCiphertext, err := codec.Seal(binding, []byte(currentSecret))
	if err != nil {
		t.Fatalf("Seal current: %v", err)
	}
	previousCiphertext, err := codec.Seal(binding, []byte(previousSecret))
	if err != nil {
		t.Fatalf("Seal previous: %v", err)
	}
	source := &InboundSource{ID: testID(42), TenantID: testID(7), Status: StatusActive}
	store := &fakeInboundSourceStore{
		source: source,
		secrets: &SigningSecrets{
			CurrentCiphertext:  currentCiphertext,
			PreviousCiphertext: previousCiphertext,
		},
	}
	svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute})
	body := []byte(`{"type":"invoice.paid"}`)
	headers := inboundTestHeaders(t, previousSecret, "evt_1", body, time.Now().UTC())

	if _, err := svc.ReceiveInbound(context.Background(), testID(42), headers, body); err != nil {
		t.Fatalf("previous secret inside the grace window must verify: %v", err)
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

// recordingAuditRecorder captures the service's domain audit events.
type recordingAuditRecorder struct {
	events []audit.Event
}

func (r *recordingAuditRecorder) Record(_ context.Context, event audit.Event) error {
	r.events = append(r.events, event)
	return nil
}

// TestReceiveInbound_RecordsTier1Action pins the Tier 1 action named by the
// route map: webhook.received, under the source's tenant.
func TestReceiveInbound_RecordsTier1Action(t *testing.T) {
	codec, err := NewSecretCodec("inbound-audit-action-test-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	secret := testInboundSecret(t)
	binding := InboundSourceSecretBinding(testID(7), testID(42))
	ciphertext, err := codec.Seal(binding, []byte(secret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	store := &fakeInboundSourceStore{
		source:  &InboundSource{ID: testID(42), TenantID: testID(7), Status: StatusActive},
		secrets: &SigningSecrets{CurrentCiphertext: ciphertext},
	}
	recorder := &recordingAuditRecorder{}
	svc := New(nil, nil, store, codec, recorder, Options{TimestampTolerance: time.Minute})
	body := []byte(`{"type":"invoice.paid"}`)
	headers := inboundTestHeaders(t, secret, "evt_1", body, time.Now().UTC())

	if _, err := svc.ReceiveInbound(context.Background(), testID(42), headers, body); err != nil {
		t.Fatalf("ReceiveInbound: %v", err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Action != "webhook.received" {
		t.Fatalf("action = %q, want %q", event.Action, "webhook.received")
	}
	if event.TenantID != testID(7) {
		t.Fatalf("tenant = %s, want the source tenant %s", event.TenantID, testID(7))
	}
	if event.Resource != "webhook_inbound_event" || event.ResourceID != testID(1).String() {
		t.Fatalf("resource = %q/%q, want webhook_inbound_event/%s", event.Resource, event.ResourceID, testID(1))
	}
}

func isUnauthorized(err error) bool {
	var appErr *apperror.AppError
	return errors.As(err, &appErr) && resolvedStatus(appErr) == http.StatusUnauthorized
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
	ciphertext, err := codec.Seal(InboundSourceSecretBinding(testID(7), testID(42)), []byte(secret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	body := []byte(`{"type":"invoice.paid"}`)
	now := time.Now().UTC()

	active := &InboundSource{ID: testID(42), TenantID: testID(7), Status: StatusActive}
	disabled := *active
	disabled.Status = StatusDisabled

	cases := map[string]*fakeInboundSourceStore{
		"unknown source":        {err: apperror.ErrNotFound},
		"disabled source":       {source: &disabled},
		"deleted source":        {err: apperror.ErrNotFound},
		"active without secret": {source: active},
		"existing but bad":      {source: active, secrets: &SigningSecrets{CurrentCiphertext: ciphertext}},
	}
	var problems []*apperror.AppError
	for name, store := range cases {
		t.Run(name, func(t *testing.T) {
			svc := New(nil, nil, store, codec, nil, Options{TimestampTolerance: time.Minute})
			headers := inboundTestHeaders(t, secret, "evt_1", body, now)
			if name == "existing but bad" {
				headers.Set(webhook.HeaderSignature, "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
			}
			_, err := svc.ReceiveInbound(context.Background(), testID(42), headers, body)
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("error = %v, want an AppError", err)
			}
			if resolvedStatus(appErr) != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resolvedStatus(appErr))
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
