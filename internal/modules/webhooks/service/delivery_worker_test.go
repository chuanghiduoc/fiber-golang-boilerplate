package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhooksqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

func TestDeliveryWorker_InvalidPayloadIsPermanent(t *testing.T) {
	codec, _ := testCodec(t)
	worker, _ := testWorker(&fakeWorkerStore{}, loopbackWebhookClient(t))
	worker.codec = codec
	for _, payload := range []string{`not-json`, `{"deliveryId":0}`, `{}`} {
		err := worker.Handle(context.Background(), json.RawMessage(payload))
		if err == nil || !jobs.IsPermanent(err) {
			t.Fatalf("Handle(%s) error = %v, want a permanent job error", payload, err)
		}
	}
}

type fakeWorkerStore struct {
	delivery   *repository.Delivery
	claimErr   error
	getErr     error
	attempts   []repository.AttemptDraft
	succeeded  bool
	dead       bool
	deadReason string
	retryDraft *repository.RetryDraft
	expire     bool
	expired    bool
	// rejectCanceled makes bookkeeping writes fail on a canceled context,
	// like a real database call would.
	rejectCanceled bool
	// activeSecret is the endpoint's current secret; nil means the snapshot
	// is still active.
	activeSecret *repository.SigningSecret
	// leaseLost makes every fenced outcome write match no row.
	leaseLost bool
	// fencedAttempts records the attempt each fenced write was fenced on.
	fencedAttempts []int32
	released       bool
	// beforeAttempt runs when the attempt is recorded, after the response.
	beforeAttempt func()
}

func (f *fakeWorkerStore) ActiveSigningSecret(context.Context, int64) (*repository.SigningSecret, error) {
	if f.activeSecret != nil {
		return f.activeSecret, nil
	}
	if f.delivery == nil {
		return nil, errors.New("not found")
	}
	return &repository.SigningSecret{Ciphertext: f.delivery.SecretCiphertext}, nil
}

func (f *fakeWorkerStore) ReleaseDeliveryClaim(ctx context.Context, _ int64, attempt int32) error {
	if f.rejectCanceled && ctx.Err() != nil {
		return ctx.Err()
	}
	f.fencedAttempts = append(f.fencedAttempts, attempt)
	if f.leaseLost {
		return repository.ErrDeliveryLeaseLost
	}
	f.released = true
	return nil
}

func (f *fakeWorkerStore) ExpireUnclaimableDelivery(context.Context, int64) (bool, error) {
	f.expired = f.expire
	return f.expire, nil
}

func (f *fakeWorkerStore) ClaimDelivery(context.Context, int64, time.Duration) (*repository.Delivery, error) {
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

func (f *fakeWorkerStore) GetDelivery(context.Context, int64) (*repository.Delivery, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.delivery == nil {
		return nil, errors.New("not found")
	}
	return f.delivery, nil
}

func (f *fakeWorkerStore) MarkDeliverySucceeded(_ context.Context, _ int64, attempt int32, _ int) error {
	f.fencedAttempts = append(f.fencedAttempts, attempt)
	if f.leaseLost {
		return repository.ErrDeliveryLeaseLost
	}
	f.succeeded = true
	return nil
}

func (f *fakeWorkerStore) MarkDeliveryDead(_ context.Context, _ int64, attempt int32, _ int, reason string) error {
	f.fencedAttempts = append(f.fencedAttempts, attempt)
	if f.leaseLost {
		return repository.ErrDeliveryLeaseLost
	}
	f.dead = true
	f.deadReason = reason
	return nil
}

func (f *fakeWorkerStore) RescheduleDeliveryWithJob(ctx context.Context, draft repository.RetryDraft) error {
	if f.rejectCanceled && ctx.Err() != nil {
		return ctx.Err()
	}
	f.fencedAttempts = append(f.fencedAttempts, draft.Attempt)
	if f.leaseLost {
		return repository.ErrDeliveryLeaseLost
	}
	f.retryDraft = &draft
	return nil
}

func (f *fakeWorkerStore) CreateDeliveryAttempt(ctx context.Context, draft repository.AttemptDraft) error {
	if f.beforeAttempt != nil {
		f.beforeAttempt()
	}
	if f.rejectCanceled && ctx.Err() != nil {
		return ctx.Err()
	}
	f.attempts = append(f.attempts, draft)
	return nil
}

func fixedDelivery(url, ciphertext string, attempts, maxAttempts int32) *repository.Delivery {
	created := time.Date(2026, 10, 24, 11, 59, 0, 0, time.UTC)
	return &repository.Delivery{
		WebhookDelivery: webhooksqlc.WebhookDelivery{
			ID:               7,
			TenantID:         1,
			EndpointID:       2,
			EventID:          "evt_1",
			Url:              url,
			SecretCiphertext: ciphertext,
			Status:           "pending",
			Attempts:         attempts,
			MaxAttempts:      maxAttempts,
			NextAttemptAt:    pgtype.Timestamptz{Time: created, Valid: true},
			CreatedAt:        pgtype.Timestamptz{Time: created, Valid: true},
			UpdatedAt:        pgtype.Timestamptz{Time: created, Valid: true},
		},
		EventType:   "file.uploaded",
		DataVersion: 1,
		ActorType:   "user",
		ActorID:     "3",
		OccurredAt:  pgtype.Timestamptz{Time: created, Valid: true},
		Payload:     []byte(`{"fileId":9}`),
	}
}

// Standard Webhooks secrets ("whsec_" + base64 key) used by the worker tests.
var (
	testSecret    = webhook.SecretPrefix + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" // public Standard Webhooks spec example key
	rotatedSecret = "whsec_" + base64.StdEncoding.EncodeToString([]byte("second-rotated-key-32-bytes-long!"))
)

// specVerify is an independent Standard Webhooks verifier written from the
// specification (not pkg/webhook): the key is the base64-decoded secret after
// "whsec_", the signed content is "<webhook-id>.<webhook-timestamp>.<body>",
// and webhook-signature holds space-separated "v1,<base64 HMAC-SHA256>"
// entries, any of which may match.
func specVerify(secret string, header http.Header, body []byte) error {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return fmt.Errorf("decode secret: %w", err)
	}
	id, timestamp := header.Get("webhook-id"), header.Get("webhook-timestamp")
	if id == "" || timestamp == "" {
		return errors.New("missing webhook-id or webhook-timestamp")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, signature := range strings.Split(header.Get("webhook-signature"), " ") {
		if hmac.Equal([]byte(signature), []byte(expected)) {
			return nil
		}
	}
	return errors.New("no matching v1 signature")
}

func testCodec(t *testing.T) (codec *SecretCodec, ciphertext string) {
	t.Helper()
	codec, err := NewSecretCodec("test-webhook-encryption-key")
	if err != nil {
		t.Fatalf("NewSecretCodec: %v", err)
	}
	ciphertext, err = codec.Seal([]byte(testSecret))
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
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	var verified bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := specVerify(testSecret, r.Header, body); err != nil {
			t.Errorf("independent Standard Webhooks verification failed: %v", err)
		} else {
			verified = true
		}
		if r.Header.Get("webhook-id") != "evt_1" || r.Header.Get("webhook-timestamp") != strconv.FormatInt(now.Unix(), 10) {
			t.Errorf("headers = %v", r.Header)
		}
		for _, legacy := range []string{"X-Webhook-Id", "X-Webhook-Event", "X-Webhook-Signature", "X-Webhook-Delivery"} {
			if r.Header.Get(legacy) != "" {
				t.Errorf("legacy header %s must not be sent", legacy)
			}
		}
		const want = `{"id":"evt_1","type":"file.uploaded","dataVersion":1,"occurredAt":"2026-10-24T11:59:00Z",` +
			`"tenantId":1,"actor":{"type":"user","id":"3"},"data":{"fileId":9}}`
		if string(body) != want {
			t.Errorf("body =\n%s\nwant\n%s", body, want)
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

	// A pending delivery that can never be claimed again (inactive endpoint
	// or exhausted attempts) is dead-lettered instead of staying pending.
	store.expire = true
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("unclaimable pending delivery should be expired: %v", err)
	}
	if !store.expired {
		t.Fatal("expected ExpireUnclaimableDelivery to dead-letter the delivery")
	}

	store.delivery.Status = "succeeded"
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("terminal delivery should be a no-op: %v", err)
	}
}

// TestDeliveryWorker_RecordsAttemptAfterShutdownCancel proves an attempt whose
// response arrived is still recorded when the job context is canceled before
// the bookkeeping writes.
func TestDeliveryWorker_RecordsAttemptAfterShutdownCancel(t *testing.T) {
	codec, ciphertext := testCodec(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	// Cancel deterministically after the response arrived; canceling inside
	// the server handler races the client reading the response.
	store := &fakeWorkerStore{delivery: fixedDelivery(server.URL, ciphertext, 0, 3), rejectCanceled: true, beforeAttempt: cancel}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
	if err := worker.Handle(ctx, payload); err != nil {
		t.Fatalf("Handle(): %v", err)
	}
	if len(store.attempts) != 1 {
		t.Fatalf("attempts recorded = %d, want 1", len(store.attempts))
	}
	if store.retryDraft == nil {
		t.Fatal("the next attempt must still be scheduled")
	}
}

// TestDeliveryWorker_AppliesEndpointTimeout proves the snapshotted endpoint
// timeout bounds the request even when the client timeout is longer.
func TestDeliveryWorker_AppliesEndpointTimeout(t *testing.T) {
	codec, ciphertext := testCodec(t)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	defer close(release)

	delivery := fixedDelivery(server.URL, ciphertext, 0, 3)
	delivery.TimeoutMs = 100
	store := &fakeWorkerStore{delivery: delivery}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})

	started := time.Now()
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("Handle(): %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("request took %s; the 100ms endpoint timeout was not applied", elapsed)
	}
	if store.succeeded || store.retryDraft == nil {
		t.Fatalf("timeout must schedule a retry: succeeded=%v retry=%v", store.succeeded, store.retryDraft)
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

// TestDeliveryWorker_ShutdownBeforeResponseReleasesAttempt proves a job
// context canceled mid-request (worker shutdown) on the final attempt neither
// records a failed attempt nor dead-letters the delivery: the attempt is given
// back and the job context error is returned so the jobs worker releases the
// job without consuming its attempt.
func TestDeliveryWorker_ShutdownBeforeResponseReleasesAttempt(t *testing.T) {
	codec, ciphertext := testCodec(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	store := &fakeWorkerStore{delivery: fixedDelivery(server.URL, ciphertext, 2, 3), rejectCanceled: true}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})

	err := worker.Handle(ctx, payload)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle() error = %v, want context.Canceled so the job is released", err)
	}
	if !store.released || len(store.fencedAttempts) != 1 || store.fencedAttempts[0] != 3 {
		t.Fatalf("claim must be released fenced on attempt 3: released=%v fenced=%v", store.released, store.fencedAttempts)
	}
	if store.dead || store.succeeded || store.retryDraft != nil {
		t.Fatalf("interrupted attempt must not finish the delivery: dead=%v succeeded=%v retry=%+v",
			store.dead, store.succeeded, store.retryDraft)
	}
	if len(store.attempts) != 0 {
		t.Fatalf("interrupted attempt must not be recorded as a failure: %+v", store.attempts)
	}
}

// TestDeliveryWorker_StaleOutcomeIsDiscarded proves outcome writes are fenced
// on the claimed attempt, and a lost lease is logged and dropped instead of
// failing (and endlessly retrying) the job.
func TestDeliveryWorker_StaleOutcomeIsDiscarded(t *testing.T) {
	codec, ciphertext := testCodec(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	for _, lost := range []bool{false, true} {
		store := &fakeWorkerStore{delivery: fixedDelivery(server.URL, ciphertext, 1, 3), leaseLost: lost}
		worker, _ := testWorker(store, loopbackWebhookClient(t))
		worker.codec = codec
		payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
		if err := worker.Handle(context.Background(), payload); err != nil {
			t.Fatalf("leaseLost=%v: Handle(): %v", lost, err)
		}
		if len(store.fencedAttempts) != 1 || store.fencedAttempts[0] != 2 {
			t.Fatalf("leaseLost=%v: outcome must be fenced on the claimed attempt 2, got %v", lost, store.fencedAttempts)
		}
		if store.succeeded == lost {
			t.Fatalf("leaseLost=%v: succeeded=%v", lost, store.succeeded)
		}
	}
}

// TestDeliveryWorker_RotationGraceCoSigns proves a retry whose snapshot
// predates a secret rotation is signed with both secrets inside the grace
// period and with the current secret alone after it.
func TestDeliveryWorker_RotationGraceCoSigns(t *testing.T) {
	codec, oldCiphertext := testCodec(t)
	newCiphertext, err := codec.Seal([]byte(rotatedSecret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		rotatedAt time.Time
		oldValid  bool
	}{
		{"inside grace", now.Add(-time.Hour), true},
		{"after grace", now.Add(-secretRotationGrace - time.Minute), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var newErr, oldErr error
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Count(r.Header.Get("webhook-signature"), "v1,") > 1 && !strings.Contains(r.Header.Get("webhook-signature"), " ") {
					t.Errorf("co-signatures must be space-separated: %q", r.Header.Get("webhook-signature"))
				}
				newErr = verifyWith(rotatedSecret, r.Header, body, now)
				oldErr = verifyWith(testSecret, r.Header, body, now)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			store := &fakeWorkerStore{
				delivery:     fixedDelivery(server.URL, oldCiphertext, 0, 3),
				activeSecret: &repository.SigningSecret{Ciphertext: newCiphertext, CreatedAt: tc.rotatedAt},
			}
			worker, _ := testWorker(store, loopbackWebhookClient(t))
			worker.codec = codec
			payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
			if err := worker.Handle(context.Background(), payload); err != nil {
				t.Fatalf("Handle(): %v", err)
			}
			if newErr != nil {
				t.Fatalf("a receiver on the rotated secret must verify: %v", newErr)
			}
			if tc.oldValid && oldErr != nil {
				t.Fatalf("a receiver still on the snapshot secret must verify inside the grace: %v", oldErr)
			}
			if !tc.oldValid && !errors.Is(oldErr, webhook.ErrInvalidSignature) {
				t.Fatalf("snapshot secret must stop signing after the grace, got %v", oldErr)
			}
		})
	}
}

// verifyWith checks a request with the production verifier (pkg/webhook).
func verifyWith(secret string, header http.Header, body []byte, now time.Time) error {
	key, err := webhook.DecodeSecret(secret)
	if err != nil {
		return err
	}
	return webhook.VerifyRequest(key, header, body, time.Minute, now)
}

// A secret created before Standard Webhooks cannot be decoded into a key: the
// delivery dead-letters with an actionable reason instead of retrying.
func TestDeliveryWorker_LegacySecretIsDead(t *testing.T) {
	codec, _ := testCodec(t)
	legacy, err := codec.Seal([]byte("whsec_Zm9v-legacy_url_safe"))
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeWorkerStore{delivery: fixedDelivery("https://example.com/hook", legacy, 0, 3)}
	worker, _ := testWorker(store, loopbackWebhookClient(t))
	worker.codec = codec
	payload, _ := json.Marshal(repository.DeliveryJobPayload{DeliveryID: 7})
	if err := worker.Handle(context.Background(), payload); err != nil {
		t.Fatalf("Handle(): %v", err)
	}
	if !store.dead || !strings.Contains(store.deadReason, "rotate") {
		t.Fatalf("dead=%v reason=%q, want a dead delivery asking for rotation", store.dead, store.deadReason)
	}
}
