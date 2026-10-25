//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
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

// wireGoldenBody and wireGoldenHeaders are the exact request the delivery
// worker sent for the fixed event below, captured from the schema that
// stored payload/event_type/event_created_at on webhook_deliveries (before
// webhook_events existed). The normalized schema must reproduce them byte for
// byte; X-Webhook-Delivery is the delivery id and X-Webhook-Timestamp /
// X-Webhook-Signature depend on the wall clock, so the test checks those
// against a signature recomputed over the golden body instead.
const wireGoldenBody = `{"id":"evt_wire_golden_1","type":"file.uploaded","createdAt":"2026-10-24T11:59:30.123456Z","data":{"big":12345678901234567890,"zeta":1,"alpha":{"nested":[1,2.50,"x"]},"unicode":"héllo \u003c\u0026\u003e"}}`

var wireGoldenHeaders = []string{
	"Accept-Encoding: gzip",
	"Content-Length: 206",
	"Content-Type: application/json",
	"User-Agent: fiber-golang-boilerplate-webhooks/1.0",
	"X-Webhook-Attempt: 1",
	"X-Webhook-Event: file.uploaded",
	"X-Webhook-Id: evt_wire_golden_1",
}

// TestWebhookDeliveryWireFormatIsStable dispatches a fixed event through the
// real job payload, repository, and worker, captures the HTTP request, and
// compares it with the golden capture.
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

	queue := jobs.NewQueue(pool)
	repo := webhookrepo.New(pool, queue)
	codec, err := webhookservice.NewSecretCodec("webhook-wire-encryption-key")
	require.NoError(t, err)
	const secret = "whsec_wire_golden_secret"
	ciphertext, err := codec.Seal([]byte(secret))
	require.NoError(t, err)

	tenantCtx := tenant.WithID(ctx, tenant.DefaultID)
	endpoint, err := repo.CreateEndpoint(tenantCtx, webhookrepo.CreateEndpointDraft{
		URL:              server.URL + "/hook",
		EventTypes:       []string{"file.*"},
		Status:           "active",
		MaxAttempts:      3,
		TimeoutMS:        2000,
		SecretCiphertext: ciphertext,
	})
	require.NoError(t, err)

	dispatcher := webhookservice.NewDispatcher(repo, queue)
	jobPayload := `{"id":"evt_wire_golden_1","type":"file.uploaded","tenantId":1,` +
		`"occurredAt":"2026-10-24T11:59:30.123456789Z",` +
		`"data":{"zeta":1,"alpha":{"nested":[1,2.50,"x"]},"unicode":"héllo <&>","big":12345678901234567890}}`
	require.NoError(t, dispatcher.HandleDispatchJob(tenantCtx, json.RawMessage(jobPayload)))

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
	deliveryJob, err := json.Marshal(webhookrepo.DeliveryJobPayload{DeliveryID: deliveryID})
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
	assert.Equal(t, wireGoldenBody, string(got.body), "body must be byte-identical to the golden capture")
	assert.Equal(t, strconv.FormatInt(deliveryID, 10), got.header.Get(webhook.HeaderDelivery))

	headers := make([]string, 0, len(got.header))
	for name, values := range got.header {
		switch name {
		case webhook.HeaderDelivery, webhook.HeaderTimestamp, webhook.HeaderSignature:
			continue
		}
		headers = append(headers, name+": "+strings.Join(values, ","))
	}
	sort.Strings(headers)
	assert.Equal(t, wireGoldenHeaders, headers)

	timestamp, err := strconv.ParseInt(got.header.Get(webhook.HeaderTimestamp), 10, 64)
	require.NoError(t, err)
	want := webhook.SignatureHeader(webhook.Sign([]byte(secret), "evt_wire_golden_1", "file.uploaded",
		time.Unix(timestamp, 0), []byte(wireGoldenBody)))
	assert.Equal(t, want, got.header.Get(webhook.HeaderSignature))
	require.NoError(t, webhook.VerifyRequest([]byte(secret), got.header, got.body, time.Minute, time.Unix(timestamp, 0)))

	// A replay is a new event (fresh id and createdAt) that must carry the
	// original type and byte-identical data.
	svc := webhookservice.New(repo, repo, repo, codec, nil, webhookservice.Options{})
	replayed, err := svc.ReplayDelivery(tenantCtx, 0, deliveryID)
	require.NoError(t, err)
	replayJob, err := json.Marshal(webhookrepo.DeliveryJobPayload{DeliveryID: replayed.ID})
	require.NoError(t, err)
	require.NoError(t, worker.Handle(tenantCtx, replayJob))
	var replay capturedRequest
	select {
	case replay = <-captured:
	case <-time.After(5 * time.Second):
		t.Fatal("receiver got no replay request")
	}
	var envelope struct {
		ID        string `json:"id"`
		CreatedAt string `json:"createdAt"`
	}
	require.NoError(t, json.Unmarshal(replay.body, &envelope))
	assert.True(t, strings.HasPrefix(envelope.ID, "replay."), "replay id %q", envelope.ID)
	_, goldenData, found := strings.Cut(wireGoldenBody, `,"data":`)
	require.True(t, found, "golden body has a data member")
	goldenData = `,"data":` + goldenData
	wantReplay := `{"id":"` + envelope.ID + `","type":"file.uploaded","createdAt":"` + envelope.CreatedAt + `"` + goldenData
	assert.Equal(t, wantReplay, string(replay.body), "replay must resend the original type and data bytes")
	assert.Equal(t, envelope.ID, replay.header.Get(webhook.HeaderID))
	assert.Equal(t, "file.uploaded", replay.header.Get(webhook.HeaderEvent))
}
