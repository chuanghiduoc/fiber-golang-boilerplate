//go:build integration

package integration_test

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
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// capturedRequest is one outbound delivery as the receiver saw it.
type capturedRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// wireSecret is the Standard Webhooks example secret ("whsec_" + base64 key).
const wireSecret = webhook.SecretPrefix + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw" // public spec example key

// verifyStandardWebhook is an independent Standard Webhooks verifier written
// from the specification (https://www.standardwebhooks.com), deliberately not
// pkg/webhook: the HMAC key is the base64-decoded secret after "whsec_", the
// signed content is "<webhook-id>.<webhook-timestamp>.<raw body>", the
// timestamp must be within five minutes of now, and webhook-signature holds
// space-separated "v1,<base64 HMAC-SHA256>" entries, any of which may match.
func verifyStandardWebhook(secret string, header http.Header, body []byte, now time.Time) error {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return fmt.Errorf("decode secret: %w", err)
	}
	id, rawTimestamp := header.Get("webhook-id"), header.Get("webhook-timestamp")
	if id == "" || rawTimestamp == "" {
		return errors.New("missing webhook-id or webhook-timestamp")
	}
	seconds, err := strconv.ParseInt(rawTimestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("webhook-timestamp: %w", err)
	}
	if skew := now.Sub(time.Unix(seconds, 0)); skew > 5*time.Minute || skew < -5*time.Minute {
		return fmt.Errorf("webhook-timestamp is %s away from now", skew)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + rawTimestamp + "."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	for _, entry := range strings.Split(header.Get("webhook-signature"), " ") {
		version, signature, _ := strings.Cut(entry, ",")
		if version == "v1" && hmac.Equal([]byte(signature), []byte(expected)) {
			return nil
		}
	}
	return errors.New("no matching v1 signature")
}

// wireGoldenBody is the exact Standard Webhooks request body the worker must
// send for the fixed event below: the versioned envelope with the
// data stored as JSONB (keys normalized by PostgreSQL, numbers kept verbatim,
// "<", "&", ">" HTML-escaped by encoding/json).
const wireGoldenBody = `{"id":"evt_wire_golden_1","type":"file.uploaded","dataVersion":1,` +
	`"occurredAt":"2026-10-24T11:59:30.123456Z","tenantId":1,"actor":{"type":"user","id":"42"},` +
	`"data":{"big":12345678901234567890,"zeta":1,"alpha":{"nested":[1,2.50,"x"]},"unicode":"héllo \u003c\u0026\u003e"}}`

// wireGoldenHeaders are the headers besides webhook-timestamp and
// webhook-signature, which depend on the clock and are verified separately.
var wireGoldenHeaders = []string{
	"Accept-Encoding: gzip",
	"Content-Length: " + strconv.Itoa(len(wireGoldenBody)),
	"Content-Type: application/json",
	"User-Agent: fiber-golang-boilerplate-webhooks/2.0",
	"Webhook-Id: evt_wire_golden_1",
}

// TestWebhookDeliveryWireFormatIsStable feeds a fixed event (exactly as the
// event bus stores it in the durable subscription job) through the real
// dispatcher, repository, and worker, captures the HTTP request, and compares
// it with the golden Standard Webhooks request.
func TestWebhookDeliveryWireFormatIsStable(t *testing.T) {
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	captured := make(chan capturedRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- capturedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	queue := jobs.NewQueue(testutil.Scoped(pool))
	repo := webhookrepo.New(testutil.Scoped(pool), testutil.NewTxManager(pool), queue)
	codec, err := webhookservice.NewSecretCodec("webhook-wire-encryption-key")
	require.NoError(t, err)
	ciphertext, err := codec.Seal([]byte(wireSecret))
	require.NoError(t, err)

	tenantCtx := tenant.WithID(ctx, tenant.DefaultID)
	endpoint, err := repo.CreateEndpoint(tenantCtx, webhookservice.CreateEndpointDraft{
		URL:              server.URL + "/hook",
		EventTypes:       []string{"file.*"},
		Status:           "active",
		MaxAttempts:      3,
		TimeoutMS:        2000,
		SecretCiphertext: ciphertext,
	})
	require.NoError(t, err)

	dispatcher := webhookservice.NewDispatcher(repo)
	jobPayload := `{"id":"evt_wire_golden_1","type":"file.uploaded","dataVersion":1,` +
		`"occurredAt":"2026-10-24T11:59:30.123456789Z","tenantId":1,"actor":{"type":"user","id":"42"},` +
		`"data":{"zeta":1,"alpha":{"nested":[1,2.50,"x"]},"unicode":"héllo <&>","big":12345678901234567890},` +
		`"requestId":"req-internal"}`
	var event events.Event
	require.NoError(t, json.Unmarshal([]byte(jobPayload), &event))
	require.NoError(t, dispatcher.HandleEvent(tenantCtx, event))

	var deliveryID int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id FROM webhook_deliveries WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = 'evt_wire_golden_1'`,
		tenant.DefaultID, endpoint.ID,
	).Scan(&deliveryID))

	guard, err := webhook.NewGuard([]string{"127.0.0.1"})
	require.NoError(t, err)
	worker := webhookservice.NewDeliveryWorker(repo, codec,
		webhook.NewClient(webhook.ClientConfig{Guard: guard, Timeout: 2 * time.Second}),
		webhookservice.WorkerOptions{RetryBase: time.Second, RetryMax: time.Minute})
	deliveryJob, err := json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: deliveryID})
	require.NoError(t, err)
	require.NoError(t, worker.Handle(tenantCtx, deliveryJob))

	var got capturedRequest
	select {
	case got = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("receiver got no request")
	}

	assert.Equal(t, http.MethodPost, got.method)
	assert.Equal(t, "/hook", got.path)
	assert.Equal(t, wireGoldenBody, string(got.body), "body must be byte-identical to the golden request")
	assert.NotContains(t, string(got.body), "req-internal", "the request id never leaves the process")

	headers := make([]string, 0, len(got.header))
	for name, values := range got.header {
		switch name {
		case "Webhook-Timestamp", "Webhook-Signature":
			continue
		}
		headers = append(headers, name+": "+strings.Join(values, ","))
	}
	sort.Strings(headers)
	assert.Equal(t, wireGoldenHeaders, headers)
	require.NoError(t, verifyStandardWebhook(wireSecret, got.header, got.body, time.Now()))

	// A replay is a new delivery row and attempt counter for the SAME message:
	// webhook-id, body id, and occurredAt stay the original ones so the
	// receiver can deduplicate it, and the envelope bytes are unchanged.
	svc := webhookservice.New(repo, repo, repo, codec, nil, webhookservice.Options{})
	replayed, err := svc.ReplayDelivery(tenantCtx, 0, deliveryID)
	require.NoError(t, err)
	require.NotEqual(t, deliveryID, replayed.ID, "a replay gets its own delivery row")
	replayJob, err := json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: replayed.ID})
	require.NoError(t, err)
	require.NoError(t, worker.Handle(tenantCtx, replayJob))
	var replay capturedRequest
	select {
	case replay = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("receiver got no replay request")
	}
	assert.Equal(t, wireGoldenBody, string(replay.body), "replay must resend the original envelope bytes")
	assert.Equal(t, "evt_wire_golden_1", replay.header.Get(webhook.HeaderID),
		"the message id is stable across replays")
	require.NoError(t, verifyStandardWebhook(wireSecret, replay.header, replay.body, time.Now()))
}
