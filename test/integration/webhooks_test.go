//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

const webhookIntegrationSecret = "webhook-integration-signing-secret"

type webhookHarness struct {
	app        *fiber.App
	pool       *pgxpool.Pool
	repo       *webhookrepo.Postgres
	service    webhookservice.Service
	dispatcher *webhookservice.Dispatcher
	worker     *webhookservice.DeliveryWorker
	codec      *webhookservice.SecretCodec
	tenantA    int64
	tenantB    int64
	tokenA     string
	tokenB     string
	tokenUser  string
	// endpointID/endpointSecret are shared between sequential subtests.
	endpointID     int64
	endpointSecret string
}

type endpointDTO struct {
	ID          int64    `json:"id"`
	TenantID    int64    `json:"tenantId"`
	URL         string   `json:"url"`
	EventTypes  []string `json:"eventTypes"`
	Status      string   `json:"status"`
	Secret      string   `json:"secret"`
	MaxAttempts int32    `json:"maxAttempts"`
}

type sourceDTO struct {
	ID          int64  `json:"id"`
	TenantID    int64  `json:"tenantId"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Status      string `json:"status"`
	Secret      string `json:"secret"`
	ReceiverURL string `json:"receiverUrl"`
}

type deliveryDTO struct {
	ID           int64  `json:"id"`
	TenantID     int64  `json:"tenantId"`
	EndpointID   int64  `json:"endpointId"`
	EventID      string `json:"eventId"`
	EventType    string `json:"eventType"`
	Status       string `json:"status"`
	Attempts     int32  `json:"attempts"`
	MaxAttempts  int32  `json:"maxAttempts"`
	LastError    string `json:"lastError"`
	ReplayedFrom *int64 `json:"replayedFromId"`
}

type receiptDTO struct {
	ID        int64  `json:"id"`
	TenantID  int64  `json:"tenantId"`
	SourceID  int64  `json:"sourceId"`
	EventID   string `json:"eventId"`
	Duplicate bool   `json:"duplicate"`
}

func setupWebhookHarness(t *testing.T) *webhookHarness {
	t.Helper()
	ctx := context.Background()

	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	var tenantB int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO tenants (slug, name) VALUES ('webhooks-acme', 'Webhooks Acme') RETURNING id`,
	).Scan(&tenantB))

	insertAdmin := func(tenantID int64, email string) int64 {
		t.Helper()
		var id int64
		require.NoError(t, pool.QueryRow(ctx,
			`INSERT INTO users (tenant_id, email, name, role) VALUES ($1, $2, $3, 'admin') RETURNING id`,
			tenantID, email, strings.Split(email, "@")[0],
		).Scan(&id))
		return id
	}
	userA := insertAdmin(tenant.DefaultID, "webhooks-a@test.com")
	userB := insertAdmin(tenantB, "webhooks-b@test.com")
	var regularUser int64
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, name, role) VALUES ($1, $2, $3, 'user') RETURNING id`,
		tenant.DefaultID, "webhooks-user@test.com", "webhooks-user",
	).Scan(&regularUser))

	tokenA, err := token.GenerateWithVersionAndTenant(userA, tenant.DefaultID, "webhooks-a@test.com", "admin", 0, webhookIntegrationSecret, 24)
	require.NoError(t, err)
	tokenB, err := token.GenerateWithVersionAndTenant(userB, tenantB, "webhooks-b@test.com", "admin", 0, webhookIntegrationSecret, 24)
	require.NoError(t, err)
	tokenUser, err := token.GenerateWithVersionAndTenant(regularUser, tenant.DefaultID, "webhooks-user@test.com", "user", 0, webhookIntegrationSecret, 24)
	require.NoError(t, err)

	appCache, err := cache.NewCache(config.CacheConfig{Driver: "memory"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = appCache.Close() })

	userRepo := identityrepo.NewUserRepository(pool)
	tenantRepo := identityrepo.NewTenantRepository(pool)
	resolver := identityadapter.NewTenantResolver(tenantRepo)
	sessionStore := session.NewStore(userRepo, appCache)
	jobQueue := jobs.NewQueue(pool)
	repo := webhookrepo.New(pool, jobQueue)
	codec, err := webhookservice.NewSecretCodec("webhook-integration-encryption-key")
	require.NoError(t, err)
	recorder := audit.NewPostgresRecorder(pool)
	svc := webhookservice.New(repo, repo, repo, codec, recorder, webhookservice.Options{
		DefaultMaxAttempts: 3,
		DefaultTimeoutMS:   2000,
		TimestampTolerance: 5 * time.Minute,
	})
	handler := webhookshandler.New(svc)
	dispatcher := webhookservice.NewDispatcher(repo)
	guard, err := webhook.NewGuard([]string{"127.0.0.1"})
	require.NoError(t, err)
	client := webhook.NewClient(webhook.ClientConfig{Guard: guard, Timeout: 2 * time.Second})
	worker := webhookservice.NewDeliveryWorker(repo, codec, client, webhookservice.WorkerOptions{
		RetryBase: 5 * time.Second,
		RetryMax:  time.Hour,
	})

	cfg := &config.Config{
		App:       config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 10},
		JWT:       config.JWTConfig{Secret: webhookIntegrationSecret, ExpireHour: 24, RefreshExpireDays: 30},
		CORS:      config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{StrictMax: 1000, StrictWindow: 60, NormalMax: 1000, NormalWindow: 60, RelaxedMax: 1000, RelaxedWindow: 60},
		Tenant:    config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{
		Cache:          appCache,
		Sessions:       sessionStore,
		Audit:          recorder,
		TenantResolver: resolver,
		Webhooks:       dispatcher,
	})
	application.Register(webhooks.New(handler))

	return &webhookHarness{
		app:        application.Fiber(),
		pool:       pool,
		repo:       repo,
		service:    svc,
		dispatcher: dispatcher,
		worker:     worker,
		codec:      codec,
		tenantA:    tenant.DefaultID,
		tenantB:    tenantB,
		tokenA:     tokenA,
		tokenB:     tokenB,
		tokenUser:  tokenUser,
	}
}

func (h *webhookHarness) do(t *testing.T, method, path, bearer string, body any, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, respBody, resp.Header.Clone()
}

func decodeJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(body, &out), "body: %s", body)
	return out
}

func (h *webhookHarness) createEndpoint(t *testing.T, bearer, url string, eventTypes ...string) endpointDTO {
	t.Helper()
	status, body, _ := h.do(t, http.MethodPost, "/api/v1/webhooks/endpoints", bearer, map[string]any{
		"url":        url,
		"eventTypes": eventTypes,
	}, nil)
	require.Equal(t, fiber.StatusCreated, status, "body: %s", body)
	return decodeJSON[endpointDTO](t, body)
}

func TestWebhooks_EndToEnd(t *testing.T) {
	h := setupWebhookHarness(t)
	ctx := context.Background()

	t.Run("endpoint CRUD, rotation, and tenant isolation", func(t *testing.T) {
		endpointA := h.createEndpoint(t, h.tokenA, "https://receiver.example/hook", "file.*")
		assert.Equal(t, h.tenantA, endpointA.TenantID)
		assert.Equal(t, "active", endpointA.Status)
		assert.True(t, strings.HasPrefix(endpointA.Secret, "whsec_"), "secret prefix: %q", endpointA.Secret)

		var ciphertext string
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT secret_ciphertext FROM webhook_endpoint_secrets WHERE endpoint_id = $1 AND tenant_id = $2 AND active`,
			endpointA.ID, h.tenantA,
		).Scan(&ciphertext))
		assert.NotEqual(t, endpointA.Secret, ciphertext, "plaintext secret must not be stored")

		status, body, _ := h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/endpoints/%d", endpointA.ID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		assert.NotContains(t, string(body), "\"secret\"")
		assert.NotContains(t, string(body), endpointA.Secret)

		// Another tenant cannot read or rotate the endpoint.
		status, _, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/endpoints/%d", endpointA.ID), h.tokenB, nil, nil)
		assert.Equal(t, fiber.StatusNotFound, status)
		status, _, _ = h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/endpoints/%d/rotate-secret", endpointA.ID), h.tokenB, nil, nil)
		assert.Equal(t, fiber.StatusNotFound, status)

		// Rotation is idempotent with the same key: the second response replays
		// the first secret and the database still has exactly one active secret.
		key := "rotate-endpoint-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		status, body, headers := h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/endpoints/%d/rotate-secret", endpointA.ID), h.tokenA, nil, map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		rotated := decodeJSON[endpointDTO](t, body)
		require.NotEmpty(t, rotated.Secret)
		assert.NotEqual(t, endpointA.Secret, rotated.Secret)

		status, replayBody, replayHeaders := h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/endpoints/%d/rotate-secret", endpointA.ID), h.tokenA, nil, map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", replayBody)
		replayed := decodeJSON[endpointDTO](t, replayBody)
		assert.Equal(t, rotated.Secret, replayed.Secret)
		assert.Equal(t, "true", replayHeaders.Get("Idempotent-Replayed"), "first response headers: %v", headers)

		var activeSecrets, versions int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE active)::int, count(*)::int FROM webhook_endpoint_secrets WHERE endpoint_id = $1 AND tenant_id = $2`,
			endpointA.ID, h.tenantA,
		).Scan(&activeSecrets, &versions))
		assert.Equal(t, 1, activeSecrets)
		assert.Equal(t, 2, versions)

		var auditCount int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM audit_events WHERE tenant_id = $1 AND action = 'webhook.endpoint.created' AND resource_id = $2`,
			h.tenantA, strconv.FormatInt(endpointA.ID, 10),
		).Scan(&auditCount))
		assert.Equal(t, 1, auditCount)

		// Lists are tenant-scoped.
		status, body, _ = h.do(t, http.MethodGet, "/api/v1/webhooks/endpoints", h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		var page struct {
			Data []endpointDTO `json:"data"`
		}
		require.NoError(t, json.Unmarshal(body, &page))
		require.Len(t, page.Data, 1)
		assert.Equal(t, endpointA.ID, page.Data[0].ID)

		status, body, _ = h.do(t, http.MethodGet, "/api/v1/webhooks/endpoints", h.tokenB, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		require.NoError(t, json.Unmarshal(body, &page))
		assert.Empty(t, page.Data)

		// Regular users and anonymous callers cannot administer webhooks.
		status, _, _ = h.do(t, http.MethodGet, "/api/v1/webhooks/endpoints", h.tokenUser, nil, nil)
		assert.Equal(t, fiber.StatusForbidden, status)
		status, _, _ = h.do(t, http.MethodGet, "/api/v1/webhooks/endpoints", "", nil, nil)
		assert.Equal(t, fiber.StatusUnauthorized, status)

		// Persist the rotated secret for the outbound worker subtest.
		h.endpointSecret = rotated.Secret
		h.endpointID = endpointA.ID
	})

	t.Run("dispatcher fanout, dedupe, and delivery isolation", func(t *testing.T) {
		endpoint2 := h.createEndpoint(t, h.tokenA, "https://receiver.example/users", "user.registered")
		event := webhookcontracts.Event{
			ID:         "evt_file_integration_1",
			Type:       "file.uploaded",
			TenantID:   h.tenantA,
			OccurredAt: time.Now().UTC(),
			Data:       json.RawMessage(`{"fileId":77}`),
		}
		created, err := h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), event)
		require.NoError(t, err)
		assert.Equal(t, 1, created)

		created, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), event)
		require.NoError(t, err)
		assert.Zero(t, created, "re-dispatching the same event must not create deliveries")

		created, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), webhookcontracts.Event{
			ID: "evt_user_integration_1", Type: "user.registered", TenantID: h.tenantA,
		})
		require.NoError(t, err)
		assert.Equal(t, 1, created)
		assert.NotZero(t, endpoint2.ID)

		var deliveries, jobsCount int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM webhook_deliveries WHERE tenant_id = $1`, h.tenantA,
		).Scan(&deliveries))
		assert.Equal(t, 2, deliveries)
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM jobs WHERE tenant_id = $1 AND kind = $2`, h.tenantA, webhookservice.DeliveryJobKind,
		).Scan(&jobsCount))
		assert.Equal(t, 2, jobsCount)

		status, body, _ := h.do(t, http.MethodGet, "/api/v1/webhooks/deliveries", h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		var page struct {
			Data []deliveryDTO `json:"data"`
		}
		require.NoError(t, json.Unmarshal(body, &page))
		assert.Len(t, page.Data, 2)

		status, body, _ = h.do(t, http.MethodGet, "/api/v1/webhooks/deliveries", h.tokenB, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		require.NoError(t, json.Unmarshal(body, &page))
		assert.Empty(t, page.Data)

		var deliveryID int64
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT id FROM webhook_deliveries WHERE tenant_id = $1 AND event_id = $2`,
			h.tenantA, "evt_file_integration_1",
		).Scan(&deliveryID))
		status, _, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/deliveries/%d", deliveryID), h.tokenB, nil, nil)
		assert.Equal(t, fiber.StatusNotFound, status)

		// Replay creates a new retained delivery rather than reusing event ID.
		status, body, _ = h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/deliveries/%d/replay", deliveryID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusCreated, status, "body: %s", body)
		replayed := decodeJSON[deliveryDTO](t, body)
		assert.NotEqual(t, deliveryID, replayed.ID)
		require.NotNil(t, replayed.ReplayedFrom)
		assert.Equal(t, deliveryID, *replayed.ReplayedFrom)

		// Pagination cursor is opaque but works for the tenant's list.
		cursor := pagination.EncodeCursor(time.Now().UTC().Add(time.Hour), 1<<40)
		status, body, _ = h.do(t, http.MethodGet, "/api/v1/webhooks/deliveries?startingAfter="+cursor, h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		require.NoError(t, json.Unmarshal(body, &page))
		assert.Len(t, page.Data, 3)
	})

	t.Run("worker success, permanent failure, retry, and signature", func(t *testing.T) {
		var mu sync.Mutex
		seen := make(map[string]int)
		var signatureValid bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			switch r.URL.Path {
			case "/success":
				secret := h.endpointSecret
				if err := webhook.VerifyRequest([]byte(secret), r.Header, body, 5*time.Minute, time.Now()); err == nil {
					mu.Lock()
					signatureValid = true
					mu.Unlock()
				}
				deliveryID := r.Header.Get(webhook.HeaderDelivery)
				mu.Lock()
				seen[deliveryID]++
				count := seen[deliveryID]
				mu.Unlock()
				if count > 1 {
					t.Logf("duplicate delivery %s deduplicated by test receiver", deliveryID)
					w.WriteHeader(http.StatusOK)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			case "/bad":
				w.WriteHeader(http.StatusBadRequest)
			case "/retry":
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusServiceUnavailable)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		successEndpoint := h.createEndpoint(t, h.tokenA, server.URL+"/success", "worker.success")
		// The success endpoint was created after the earlier rotation, so use
		// its own secret for signature verification.
		h.endpointSecret = successEndpoint.Secret

		successEvent := webhookcontracts.Event{ID: "evt_worker_success", Type: "worker.success", TenantID: h.tenantA}
		created, err := h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), successEvent)
		require.NoError(t, err)
		require.Equal(t, 1, created)

		deliveryID := h.deliveryID(t, successEvent.ID)
		payload, _ := json.Marshal(webhookrepo.DeliveryJobPayload{DeliveryID: deliveryID})
		require.NoError(t, h.worker.Handle(tenant.WithID(ctx, h.tenantA), payload))
		status, body, _ := h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/deliveries/%d", deliveryID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		success := decodeJSON[deliveryDTO](t, body)
		assert.Equal(t, "succeeded", success.Status)
		assert.Equal(t, int32(1), success.Attempts)
		mu.Lock()
		assert.True(t, signatureValid, "receiver must verify the snapshot secret signature")
		mu.Unlock()

		var attemptCount, attemptStatus int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int, max(status_code)::int FROM webhook_delivery_attempts WHERE delivery_id = $1`,
			deliveryID,
		).Scan(&attemptCount, &attemptStatus))
		assert.Equal(t, 1, attemptCount)
		assert.Equal(t, http.StatusNoContent, attemptStatus)

		status, body, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/deliveries/%d/attempts", deliveryID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		var attemptsPage struct {
			Data []struct {
				Attempt    int32 `json:"attempt"`
				StatusCode *int  `json:"statusCode"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(body, &attemptsPage))
		require.Len(t, attemptsPage.Data, 1)
		assert.Equal(t, int32(1), attemptsPage.Data[0].Attempt)
		if assert.NotNil(t, attemptsPage.Data[0].StatusCode) {
			assert.Equal(t, http.StatusNoContent, *attemptsPage.Data[0].StatusCode)
		}

		badEndpoint := h.createEndpoint(t, h.tokenA, server.URL+"/bad", "worker.permanent")
		_ = badEndpoint
		badEvent := webhookcontracts.Event{ID: "evt_worker_bad", Type: "worker.permanent", TenantID: h.tenantA}
		_, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), badEvent)
		require.NoError(t, err)
		badID := h.deliveryID(t, badEvent.ID)
		payload, _ = json.Marshal(webhookrepo.DeliveryJobPayload{DeliveryID: badID})
		require.NoError(t, h.worker.Handle(tenant.WithID(ctx, h.tenantA), payload))
		status, body, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/deliveries/%d", badID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		bad := decodeJSON[deliveryDTO](t, body)
		assert.Equal(t, "dead", bad.Status)
		assert.Contains(t, bad.LastError, "400")

		retryEndpoint := h.createEndpoint(t, h.tokenA, server.URL+"/retry", "worker.retry")
		_ = retryEndpoint
		retryEvent := webhookcontracts.Event{ID: "evt_worker_retry", Type: "worker.retry", TenantID: h.tenantA}
		_, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), retryEvent)
		require.NoError(t, err)
		retryID := h.deliveryID(t, retryEvent.ID)
		payload, _ = json.Marshal(webhookrepo.DeliveryJobPayload{DeliveryID: retryID})
		require.NoError(t, h.worker.Handle(tenant.WithID(ctx, h.tenantA), payload))
		status, body, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/deliveries/%d", retryID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		retryDelivery := decodeJSON[deliveryDTO](t, body)
		assert.Equal(t, "pending", retryDelivery.Status)
		assert.Equal(t, int32(1), retryDelivery.Attempts)

		var nextAttempt time.Time
		var lastStatus int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT next_attempt_at, last_status_code FROM webhook_deliveries WHERE id = $1`,
			retryID,
		).Scan(&nextAttempt, &lastStatus))
		assert.WithinDuration(t, time.Now().Add(30*time.Second), nextAttempt, 5*time.Second)
		assert.Equal(t, http.StatusServiceUnavailable, lastStatus)

		var retryJobs int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM jobs WHERE tenant_id = $1 AND kind = $2 AND payload->>'deliveryId' = $3`,
			h.tenantA, webhookservice.DeliveryJobKind, strconv.FormatInt(retryID, 10),
		).Scan(&retryJobs))
		assert.GreaterOrEqual(t, retryJobs, 2, "original job plus scheduled retry")
	})

	t.Run("inbound HMAC receiver, dedupe, rotation, and tenant isolation", func(t *testing.T) {
		status, body, _ := h.do(t, http.MethodPost, "/api/v1/webhooks/inbound-sources", h.tokenA, map[string]any{
			"name": "Stripe", "slug": "stripe",
		}, nil)
		require.Equal(t, fiber.StatusCreated, status, "body: %s", body)
		sourceA := decodeJSON[sourceDTO](t, body)
		require.NotEmpty(t, sourceA.Secret)
		assert.Contains(t, sourceA.ReceiverURL, "/api/v1/webhooks/inbound/")

		status, _, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/inbound-sources/%d", sourceA.ID), h.tokenB, nil, nil)
		assert.Equal(t, fiber.StatusNotFound, status)

		payload := []byte(`{"invoice":"in_1","amount":100}`)
		headers := func(secret, eventID string) map[string]string {
			ts := time.Now().UTC()
			return map[string]string{
				webhook.HeaderID:        eventID,
				webhook.HeaderEvent:     "invoice.paid",
				webhook.HeaderTimestamp: strconv.FormatInt(ts.Unix(), 10),
				webhook.HeaderSignature: webhook.SignatureHeader(webhook.Sign([]byte(secret), ts, payload)),
			}
		}

		bad := headers(sourceA.Secret, "evt_in_bad")
		bad[webhook.HeaderSignature] = "v1=" + strings.Repeat("0", 64)
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, bad)
		assert.Equal(t, fiber.StatusUnauthorized, status)

		status, body, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(sourceA.Secret, "evt_in_1"))
		require.Equal(t, fiber.StatusAccepted, status, "body: %s", body)
		first := decodeJSON[receiptDTO](t, body)
		assert.False(t, first.Duplicate)
		assert.Equal(t, h.tenantA, first.TenantID)

		status, body, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(sourceA.Secret, "evt_in_1"))
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		duplicate := decodeJSON[receiptDTO](t, body)
		assert.True(t, duplicate.Duplicate)
		assert.Equal(t, first.ID, duplicate.ID)

		var eventCount int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM webhook_inbound_events WHERE tenant_id = $1 AND source_id = $2`,
			h.tenantA, sourceA.ID,
		).Scan(&eventCount))
		assert.Equal(t, 1, eventCount)

		status, body, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/inbound-sources/%d/events", sourceA.ID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		var page struct {
			Data []struct {
				EventID string `json:"eventId"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(body, &page))
		require.Len(t, page.Data, 1)
		assert.Equal(t, "evt_in_1", page.Data[0].EventID)

		// Rotation invalidates the old secret immediately.
		key := "rotate-source-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		status, body, _ = h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/inbound-sources/%d/rotate-secret", sourceA.ID), h.tokenA, nil, map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		rotated := decodeJSON[sourceDTO](t, body)
		require.NotEmpty(t, rotated.Secret)

		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(sourceA.Secret, "evt_in_old"))
		assert.Equal(t, fiber.StatusUnauthorized, status)
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(rotated.Secret, "evt_in_new"))
		assert.Equal(t, fiber.StatusAccepted, status)

		// A source in tenant B is reachable publicly but stores/audits under B.
		status, body, _ = h.do(t, http.MethodPost, "/api/v1/webhooks/inbound-sources", h.tokenB, map[string]any{
			"name": "GitHub", "slug": "github",
		}, nil)
		require.Equal(t, fiber.StatusCreated, status, "body: %s", body)
		sourceB := decodeJSON[sourceDTO](t, body)
		assert.Equal(t, h.tenantB, sourceB.TenantID)

		status, body, _ = h.doBytes(t, http.MethodPost, sourceB.ReceiverURL, payload, headers(sourceB.Secret, "evt_in_b"))
		require.Equal(t, fiber.StatusAccepted, status, "body: %s", body)
		receivedB := decodeJSON[receiptDTO](t, body)
		assert.Equal(t, h.tenantB, receivedB.TenantID)

		var auditB int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM audit_events WHERE tenant_id = $1 AND action = 'webhook.inbound.received'`,
			h.tenantB,
		).Scan(&auditB))
		assert.Equal(t, 1, auditB)
	})
}

func (h *webhookHarness) doBytes(t *testing.T, method, path string, body []byte, headers map[string]string) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, respBody, resp.Header.Clone()
}

func (h *webhookHarness) deliveryID(t *testing.T, eventID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, h.pool.QueryRow(context.Background(),
		`SELECT id FROM webhook_deliveries WHERE tenant_id = $1 AND event_id = $2`,
		h.tenantA, eventID,
	).Scan(&id))
	return id
}
