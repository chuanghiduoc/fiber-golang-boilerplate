//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

// TestWebhooks_FileUploadEndToEnd follows one domain event through the whole
// backbone with production wiring: a file upload publishes file.uploaded in
// its transaction, the webhooks module's durable subscription (registered by
// RegisterEvents) runs on a real jobs worker and fans out to the endpoint, the
// delivery job signs and sends the request, and the receiver checks the
// Standard Webhooks headers, signature, and envelope body independently.
func TestWebhooks_FileUploadEndToEnd(t *testing.T) {
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
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	// Infrastructure and the webhooks module, built like cmd/api does.
	const encryptionKey = "webhook-e2e-encryption-key"
	queue := jobs.NewQueue(testutil.Scoped(pool))
	bus := events.New(queue)
	recorder := audit.NewPostgresRecorder(testutil.Scoped(pool))
	module, err := webhooks.Build(app.Infra{
		Pool: pool, DB: testutil.Scoped(pool), Tx: testutil.NewTxManager(pool),
		Queue: queue, Audit: recorder, Events: bus,
	}, webhooks.Settings{
		Webhook: config.WebhookConfig{
			ConnectTimeoutSeconds:     2,
			RequestTimeoutSeconds:     5,
			RetryBaseSeconds:          1,
			RetryMaxSeconds:           5,
			MaxAttempts:               3,
			RetentionDays:             30,
			TimestampToleranceSeconds: 300,
			SSRFAllowlist:             "127.0.0.1",
			AllowedPortsCSV:           serverURL.Port(),
		},
		SecretKey: encryptionKey,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(module.Close)
	module.RegisterEvents(bus)
	worker := jobs.NewWorker(testutil.Scoped(pool), "webhooks-e2e", jobs.WithPollInterval(50*time.Millisecond))
	module.RegisterJobs(worker)
	require.NoError(t, bus.RegisterJobs(worker))

	// A tenant endpoint subscribed to file events, with a known secret.
	codec, err := webhookservice.NewSecretCodec(encryptionKey)
	require.NoError(t, err)
	tenantCtx := tenant.WithID(ctx, tenant.DefaultID)
	_, err = webhookrepo.New(testutil.Scoped(pool), testutil.NewTxManager(pool), queue).CreateEndpoint(tenantCtx, webhookservice.CreateEndpointDraft{
		URL:         server.URL + "/hook",
		EventTypes:  []string{"file.*"},
		Status:      "active",
		MaxAttempts: 3,
		TimeoutMS:   2000,
		SealSecret: func(endpointID int64) (string, error) {
			return codec.Seal(webhookservice.EndpointSecretBinding(tenant.DefaultID, endpointID), []byte(wireSecret))
		},
	})
	require.NoError(t, err)

	// The producer: a user uploads a file through the files service.
	userID := testutil.CreateMember(t, ctx, pool, tenant.DefaultID, "e2e@test.com", testutil.MemberOptions{}).UserID
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	uploads := filesvc.NewUploadService(filesrepo.NewFileRepository(testutil.Scoped(pool)),
		filesrepo.NewTransactor(testutil.NewTxManager(pool), recorder, bus), store, filesvc.UploadOptions{})
	userCtx := requestctx.WithActorID(authz.WithSubject(tenantCtx, authz.NewSubject(userID, tenant.DefaultID, "user")), userID)
	file, err := uploads.Upload(userCtx, userID, "report.pdf", strings.NewReader("%PDF-1"), 6, "application/pdf")
	require.NoError(t, err)

	// The event was published in the upload transaction: its durable
	// subscription job is already committed before any worker runs.
	var subscriberJobs int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*)::int FROM jobs WHERE tenant_id = $1 AND kind = 'events.file.uploaded.webhooks'`,
		tenant.DefaultID,
	).Scan(&subscriberJobs))
	require.Equal(t, 1, subscriberJobs)

	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		stopWorker()
		require.NoError(t, <-workerDone)
	})

	var got capturedRequest
	select {
	case got = <-captured:
	case <-time.After(30 * time.Second):
		t.Fatal("receiver got no webhook request")
	}

	// Standard Webhooks headers and a valid signature, checked by an
	// independent verifier.
	assert.Equal(t, http.MethodPost, got.method)
	assert.Equal(t, "/hook", got.path)
	require.NoError(t, verifyStandardWebhook(wireSecret, got.header, got.body, time.Now()))
	timestamp, err := strconv.ParseInt(got.header.Get("webhook-timestamp"), 10, 64)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), time.Unix(timestamp, 0), time.Minute)
	assert.Empty(t, got.header.Get("X-Webhook-Event"), "the event type is in the body, not a header")

	// The body is the versioned envelope with the contracts payload.
	var envelope struct {
		ID          string                      `json:"id"`
		Type        string                      `json:"type"`
		DataVersion int                         `json:"dataVersion"`
		OccurredAt  time.Time                   `json:"occurredAt"`
		TenantID    int64                       `json:"tenantId"`
		Actor       events.Actor                `json:"actor"`
		Data        filescontracts.FileUploaded `json:"data"`
		RequestID   *string                     `json:"requestId"`
	}
	require.NoError(t, json.Unmarshal(got.body, &envelope))
	assert.Equal(t, got.header.Get(webhook.HeaderID), envelope.ID, "webhook-id is the event id")
	id, err := uuid.Parse(envelope.ID)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(7), id.Version())
	assert.Equal(t, filescontracts.EventFileUploaded, envelope.Type)
	assert.Equal(t, filescontracts.FileUploadedVersion, envelope.DataVersion)
	assert.Equal(t, tenant.DefaultID, envelope.TenantID)
	assert.Equal(t, events.UserActor(userID), envelope.Actor)
	assert.Nil(t, envelope.RequestID)
	assert.WithinDuration(t, time.Now(), envelope.OccurredAt, time.Minute)
	assert.Equal(t, filescontracts.FileUploaded{
		FileID:       file.ID,
		OwnerID:      userID,
		OriginalName: "report.pdf",
		MimeType:     "application/pdf",
		Size:         6,
		CreatedAt:    envelope.Data.CreatedAt,
	}, envelope.Data)

	// The delivery is recorded as succeeded with the same event id.
	require.Eventually(t, func() bool {
		var status string
		err := pool.QueryRow(ctx,
			`SELECT status FROM webhook_deliveries WHERE tenant_id = $1 AND event_id = $2`,
			tenant.DefaultID, envelope.ID,
		).Scan(&status)
		return err == nil && status == "succeeded"
	}, 10*time.Second, 50*time.Millisecond)
}
