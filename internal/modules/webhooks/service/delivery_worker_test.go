package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

type fakeWorkerStore struct {
	delivery   *webhooksqlc.WebhookDelivery
	claimErr   error
	getErr     error
	attempts   []repository.AttemptDraft
	succeeded  bool
	dead       bool
	deadReason string
	retryDraft *repository.RetryDraft
}

func (f *fakeWorkerStore) ClaimDelivery(context.Context, int64, time.Duration) (*webhooksqlc.WebhookDelivery, error) {
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	if f.delivery == nil {
		return nil, repository.ErrNotClaimable
	}
	clone := *f.delivery
	clone.Attempts++
	return &clone, nil
}

func (f *fakeWorkerStore) GetDelivery(context.Context, int64) (*webhooksqlc.WebhookDelivery, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.delivery == nil {
		return nil, errors.New("not found")
	}
	return f.delivery, nil
}

func (f *fakeWorkerStore) MarkDeliverySucceeded(context.Context, int64, int) error {
	f.succeeded = true
	return nil
}

func (f *fakeWorkerStore) MarkDeliveryDead(_ context.Context, _ int64, _ int, reason string) error {
	f.dead = true
	f.deadReason = reason
	return nil
}

func (f *fakeWorkerStore) RescheduleDeliveryWithJob(_ context.Context, draft repository.RetryDraft) error {
	f.retryDraft = &draft
	return nil
}

func (f *fakeWorkerStore) CreateDeliveryAttempt(_ context.Context, draft repository.AttemptDraft) error {
	f.attempts = append(f.attempts, draft)
	return nil
}

func fixedDelivery(url, ciphertext string, attempts, maxAttempts int32) *webhooksqlc.WebhookDelivery {
	created := time.Date(2026, 10, 24, 11, 59, 0, 0, time.UTC)
	return &webhooksqlc.WebhookDelivery{
		ID:               7,
		TenantID:         1,
		EndpointID:       2,
		EventID:          "evt_1",
		EventType:        "file.uploaded",
		EventCreatedAt:   pgtype.Timestamptz{Time: created, Valid: true},
		Url:              url,
		Payload:          []byte(`{"fileId":9}`),
		SecretCiphertext: ciphertext,
		Status:           "pending",
		Attempts:         attempts,
		MaxAttempts:      maxAttempts,
		NextAttemptAt:    pgtype.Timestamptz{Time: created, Valid: true},
		CreatedAt:        pgtype.Timestamptz{Time: created, Valid: true},
		UpdatedAt:        pgtype.Timestamptz{Time: created, Valid: true},
	}
}

func testCodec(t *testing.T) (codec *SecretCodec, ciphertext string) {
	t.Helper()
	codec, err := NewSecretCodec("test-webhook-encryption-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	secret := "whsec_test_secret"
	ciphertext, err = codec.Seal([]byte(secret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return codec, ciphertext
}

func loopbackWebhookClient(t *testing.T) *webhook.Client {
	t.Helper()
	guard, err := webhook.NewGuard([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return webhook.NewClient(webhook.ClientConfig{Guard: guard, Timeout: 2 * time.Second})
}

func testWorker(store WorkerStore, client *webhook.Client) (*DeliveryWorker, time.Time) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	worker := NewDeliveryWorker(store, nil, client, WorkerOptions{
		RetryBase: 5 * time.Second,
		RetryMax:  time.Hour,
	})
	worker.now = func() time.Time { return now }
	return worker, now
}

func TestDeliveryWorker_SuccessSignsAndRecordsAttempt(t *testing.T) {
	codec, ciphertext := testCodec(t)
	secret := "whsec_test_secret"
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	var verified bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := webhook.VerifyRequest([]byte(secret), r.Header, body, time.Minute, now); err != nil {
			t.Errorf("receiver signature verification failed: %v", err)
		} else {
			verified = true
		}
		if r.Header.Get(webhook.HeaderDelivery) != "7" {
			t.Errorf("delivery header = %q, want 7", r.Header.Get(webhook.HeaderDelivery))
		}
		if r.Header.Get(webhook.HeaderID) != "evt_1" {
			t.Errorf("event header = %q", r.Header.Get(webhook.HeaderID))
		}
		var envelope DeliveryEnvelope
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("envelope: %v", err)
		}
		if envelope.ID != "evt_1" || envelope.Type != "file.uploaded" || string(envelope.Data) != `{"fileId":9}` {
			t.Errorf("envelope = %+v", envelope)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	store := &fakeWorkerStore{delivery: fixedDelivery(server.URL, ciphertext, 0, 3)}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("Handle(): %v", err)
	}
	if !verified {
		t.Fatal("receiver did not verify the signature")
	}
	if !store.succeeded || store.dead || store.retryDraft != nil {
		t.Fatalf("outcome: succeeded=%v dead=%v retry=%+v", store.succeeded, store.dead, store.retryDraft)
	}
	if len(store.attempts) != 1 || store.attempts[0].StatusCode != http.StatusNoContent {
		t.Fatalf("attempts = %+v", store.attempts)
	}
}

func TestDeliveryWorker_PermanentFailureIsDead(t *testing.T) {
	codec, ciphertext := testCodec(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	store := &fakeWorkerStore{delivery: fixedDelivery(server.URL, ciphertext, 0, 3)}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("Handle(): %v", err)
	}
	if !store.dead || store.succeeded || store.retryDraft != nil {
		t.Fatalf("outcome: dead=%v succeeded=%v retry=%+v", store.dead, store.succeeded, store.retryDraft)
	}
	if !strings.Contains(store.deadReason, "400") {
		t.Fatalf("dead reason = %q", store.deadReason)
	}
}

func TestDeliveryWorker_RetryAfterSchedulesNextJob(t *testing.T) {
	codec, ciphertext := testCodec(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	store := &fakeWorkerStore{delivery: fixedDelivery(server.URL, ciphertext, 0, 3)}
	worker, now := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("Handle(): %v", err)
	}
	if store.dead || store.succeeded {
		t.Fatalf("outcome: dead=%v succeeded=%v", store.dead, store.succeeded)
	}
	if store.retryDraft == nil {
		t.Fatal("expected a scheduled retry")
	}
	if got := store.retryDraft.NextAttemptAt.Sub(now); got != 120*time.Second {
		t.Fatalf("retry delay = %s, want 120s", got)
	}
	if store.retryDraft.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("retry status = %d", store.retryDraft.StatusCode)
	}
}

func TestDeliveryWorker_ExhaustedRetriesAreDead(t *testing.T) {
	codec, ciphertext := testCodec(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	store := &fakeWorkerStore{delivery: fixedDelivery(server.URL, ciphertext, 2, 3)}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("Handle(): %v", err)
	}
	if !store.dead || store.retryDraft != nil {
		t.Fatalf("outcome: dead=%v retry=%+v", store.dead, store.retryDraft)
	}
}

func TestDeliveryWorker_RedirectAndBlockedTargetAreDead(t *testing.T) {
	codec, ciphertext := testCodec(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer redirect.Close()

	t.Run("redirect", func(t *testing.T) {
		store := &fakeWorkerStore{delivery: fixedDelivery(redirect.URL, ciphertext, 0, 3)}
		worker, _ := testWorker(store, loopbackWebhookClient(t))
		worker.codec = codec
		payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
		if err := worker.Handle(context.Background(), payload); err != nil {
			t.Fatalf("Handle(): %v", err)
		}
		if !store.dead {
			t.Fatal("3xx must mark the delivery dead")
		}
	})

	t.Run("blocked target", func(t *testing.T) {
		guard, _ := webhook.NewGuard(nil)
		client := webhook.NewClient(webhook.ClientConfig{Guard: guard, Timeout: time.Second})
		store := &fakeWorkerStore{delivery: fixedDelivery(redirect.URL, ciphertext, 0, 3)}
		worker, _ := testWorker(store, client)
		worker.codec = codec
		payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
		if err := worker.Handle(context.Background(), payload); err != nil {
			t.Fatalf("Handle(): %v", err)
		}
		if !store.dead || !strings.Contains(store.deadReason, "blocked") {
			t.Fatalf("blocked target outcome: dead=%v reason=%q", store.dead, store.deadReason)
		}
	})
}

func TestDeliveryWorker_NotClaimable(t *testing.T) {
	codec, ciphertext := testCodec(t)
	store := &fakeWorkerStore{
		delivery: fixedDelivery("https://example.com/hook", ciphertext, 0, 3),
		claimErr: repository.ErrNotClaimable,
	}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})

	if err := worker.Handle(context.Background(), payload); err == nil {
		t.Fatal("pending delivery with an active claim should retry the job")
	}

	store.delivery.Status = "succeeded"
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("terminal delivery should be a no-op: %v", err)
	}
}

func TestDeliveryWorker_InvalidPayload(t *testing.T) {
	codec, ciphertext := testCodec(t)
	store := &fakeWorkerStore{delivery: fixedDelivery("https://example.com/hook", ciphertext, 0, 3)}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	if err := worker.Handle(context.Background(), json.RawMessage(`not-json`)); err == nil {
		t.Fatal("invalid job payload must error")
	}
}
