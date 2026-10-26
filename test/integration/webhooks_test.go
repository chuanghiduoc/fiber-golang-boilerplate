//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	identityadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/adapter"
	identityrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks"
	webhookshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/handler"
	webhookrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/repository"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/webhook"
)

const webhookIntegrationSecret = "webhook-integration-signing-secret"

// allDynamicPorts lets integration receivers listen on ephemeral ports. The
// outbound port policy itself is unit-tested in pkg/webhook.
func allDynamicPorts() []int {
	ports := make([]int, 0, 65535)
	for port := 1; port <= 65535; port++ {
		ports = append(ports, port)
	}
	return ports
}

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

	tenantB := testutil.CreateTenant(t, ctx, pool, "webhooks-acme")

	// The session store validates every token against a live session row.
	sessionToken := func(tenantID int64, email, role string) string {
		t.Helper()
		member := testutil.CreateMember(t, ctx, pool, tenantID, email, testutil.MemberOptions{Role: role})
		return testutil.SessionToken(t, ctx, pool, member, webhookIntegrationSecret)
	}
	tokenA := sessionToken(tenant.DefaultID, "webhooks-a@test.com", "admin")
	tokenB := sessionToken(tenantB, "webhooks-b@test.com", "admin")
	tokenUser := sessionToken(tenant.DefaultID, "webhooks-user@test.com", "user")

	appCache, err := cache.NewCache(config.CacheConfig{Driver: "memory"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = appCache.Close() })

	stores := identityrepo.NewStores(testutil.Scoped(pool))
	tenantRepo := stores.Tenants
	resolver := identityadapter.NewTenantResolver(tenantRepo)
	sessionStore := session.NewStore(identityadapter.NewSessionStateReader(stores.Sessions), appCache)
	jobQueue := jobs.NewQueue(testutil.Scoped(pool))
	repo := webhookrepo.New(testutil.Scoped(pool), testutil.NewTxManager(pool), jobQueue)
	codec, err := webhookservice.NewSecretCodec("webhook-integration-encryption-key")
	require.NoError(t, err)
	recorder := audit.NewPostgresRecorder(testutil.Scoped(pool))
	// httptest receivers listen on random ports; this harness exercises
	// delivery behavior, not the port policy (covered by pkg/webhook).
	guard, err := webhook.NewGuardWithPorts([]string{"127.0.0.1"}, allDynamicPorts())
	require.NoError(t, err)
	svc := webhookservice.New(repo, repo, repo, codec, recorder, webhookservice.Options{
		DefaultMaxAttempts: 3,
		DefaultTimeoutMS:   2000,
		TimestampTolerance: 5 * time.Minute,
		URLGuard:           guard,
		Tenants: webhookservice.TenantCheckerFunc(func(ctx context.Context, id int64) (bool, error) {
			if _, err := tenantRepo.GetOperationalByID(ctx, id); err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}),
	})
	handler := webhookshandler.New(svc)
	dispatcher := webhookservice.NewDispatcher(repo)
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

func (h *webhookHarness) do(t *testing.T, method, path, bearer string, body any, headers map[string]string) (status int, payload []byte, header http.Header) {
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

		// Rotation is never served from the idempotency cache (the plaintext
		// secret must not be stored there): a repeated key rotates again and
		// the database still has exactly one active secret.
		key := "rotate-endpoint-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		status, body, _ = h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/endpoints/%d/rotate-secret", endpointA.ID), h.tokenA, nil, map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		rotated := decodeJSON[endpointDTO](t, body)
		require.NotEmpty(t, rotated.Secret)
		assert.NotEqual(t, endpointA.Secret, rotated.Secret)

		status, retryBody, retryHeaders := h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/endpoints/%d/rotate-secret", endpointA.ID), h.tokenA, nil, map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", retryBody)
		retried := decodeJSON[endpointDTO](t, retryBody)
		require.NotEmpty(t, retried.Secret)
		assert.NotEqual(t, rotated.Secret, retried.Secret)
		assert.Empty(t, retryHeaders.Get("Idempotent-Replayed"))

		var activeSecrets, versions int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE active)::int, count(*)::int FROM webhook_endpoint_secrets WHERE endpoint_id = $1 AND tenant_id = $2`,
			endpointA.ID, h.tenantA,
		).Scan(&activeSecrets, &versions))
		assert.Equal(t, 1, activeSecrets)
		assert.Equal(t, 3, versions)

		// Each rotation is guarded: a succeeded intent row plus the service's
		// domain event linked to it as a child.
		var intents, children int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE parent_id IS NULL AND outcome = 'succeeded')::int,
			        count(*) FILTER (WHERE parent_id IS NOT NULL)::int
			 FROM audit_events WHERE tenant_id = $1 AND action = 'webhook.endpoint.secret_rotated'`,
			h.tenantA,
		).Scan(&intents, &children))
		assert.Equal(t, 2, intents)
		assert.Equal(t, 2, children)

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
		h.endpointSecret = retried.Secret
		h.endpointID = endpointA.ID
	})

	t.Run("dispatcher fanout, dedupe, and delivery isolation", func(t *testing.T) {
		endpoint2 := h.createEndpoint(t, h.tokenA, "https://receiver.example/users", "user.registered")
		event := testEnvelope("evt_file_integration_1", "file.uploaded", h.tenantA)
		event.Data = json.RawMessage(`{"fileId":77}`)
		created, err := h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), event)
		require.NoError(t, err)
		assert.Equal(t, 1, created)

		created, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), event)
		require.NoError(t, err)
		assert.Zero(t, created, "re-dispatching the same event must not create deliveries")

		created, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA),
			testEnvelope("evt_user_integration_1", "user.registered", h.tenantA))
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
				if err := verifyStandardWebhook(h.endpointSecret, r.Header, body, time.Now()); err == nil {
					mu.Lock()
					signatureValid = true
					mu.Unlock()
				}
				// Standard Webhooks receivers deduplicate on webhook-id.
				messageID := r.Header.Get(webhook.HeaderID)
				mu.Lock()
				seen[messageID]++
				count := seen[messageID]
				mu.Unlock()
				if count > 1 {
					t.Logf("duplicate message %s deduplicated by test receiver", messageID)
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

		successEvent := testEnvelope("evt_worker_success", "worker.success", h.tenantA)
		created, err := h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), successEvent)
		require.NoError(t, err)
		require.Equal(t, 1, created)

		deliveryID := h.deliveryID(t, successEvent.ID)
		payload, _ := json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: deliveryID})
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
		badEvent := testEnvelope("evt_worker_bad", "worker.permanent", h.tenantA)
		_, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), badEvent)
		require.NoError(t, err)
		badID := h.deliveryID(t, badEvent.ID)
		payload, _ = json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: badID})
		require.NoError(t, h.worker.Handle(tenant.WithID(ctx, h.tenantA), payload))
		status, body, _ = h.do(t, http.MethodGet, fmt.Sprintf("/api/v1/webhooks/deliveries/%d", badID), h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status)
		bad := decodeJSON[deliveryDTO](t, body)
		assert.Equal(t, "dead", bad.Status)
		assert.Contains(t, bad.LastError, "400")

		retryEndpoint := h.createEndpoint(t, h.tokenA, server.URL+"/retry", "worker.retry")
		_ = retryEndpoint
		retryEvent := testEnvelope("evt_worker_retry", "worker.retry", h.tenantA)
		_, err = h.dispatcher.Dispatch(tenant.WithID(ctx, h.tenantA), retryEvent)
		require.NoError(t, err)
		retryID := h.deliveryID(t, retryEvent.ID)
		payload, _ = json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: retryID})
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

		// Standard Webhooks senders put the event type in the signed body.
		require.True(t, strings.HasPrefix(sourceA.Secret, webhook.SecretPrefix), "inbound secret %q", sourceA.Secret)
		payload := []byte(`{"type":"invoice.paid","data":{"invoice":"in_1","amount":100}}`)
		signFor := func(secret, eventID string, body []byte) map[string]string {
			key, err := webhook.DecodeSecret(secret)
			require.NoError(t, err)
			ts := time.Now().UTC()
			return map[string]string{
				webhook.HeaderID:        eventID,
				webhook.HeaderTimestamp: strconv.FormatInt(ts.Unix(), 10),
				webhook.HeaderSignature: webhook.Sign(key, eventID, ts, body),
			}
		}
		headers := func(secret, eventID string) map[string]string {
			return signFor(secret, eventID, payload)
		}

		bad := headers(sourceA.Secret, "evt_in_bad")
		bad[webhook.HeaderSignature] = "v1," + base64.StdEncoding.EncodeToString(make([]byte, 32))
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
		var storedType string
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT event_type FROM webhook_inbound_events WHERE tenant_id = $1 AND source_id = $2`,
			h.tenantA, sourceA.ID,
		).Scan(&storedType))
		assert.Equal(t, "invoice.paid", storedType, "the type comes from the signed body")

		// Rotation keeps the previous secret valid for the grace window, so
		// senders that have not switched yet keep working.
		key := "rotate-source-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		status, body, _ = h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/inbound-sources/%d/rotate-secret", sourceA.ID), h.tokenA, nil, map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		rotated := decodeJSON[sourceDTO](t, body)
		require.NotEmpty(t, rotated.Secret)

		var previousExpiry *time.Time
		var previousCiphertext *string
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT previous_secret_ciphertext, previous_secret_expires_at
			 FROM webhook_inbound_sources WHERE id = $1 AND tenant_id = $2`,
			sourceA.ID, h.tenantA,
		).Scan(&previousCiphertext, &previousExpiry))
		require.NotNil(t, previousCiphertext, "the rotated-away secret must be kept for the grace window")
		require.NotNil(t, previousExpiry)
		assert.WithinDuration(t, time.Now().Add(24*time.Hour), *previousExpiry, time.Minute)

		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(sourceA.Secret, "evt_in_old"))
		assert.Equal(t, fiber.StatusAccepted, status, "the previous secret must verify during the grace window")
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(rotated.Secret, "evt_in_new"))
		assert.Equal(t, fiber.StatusAccepted, status)

		// A second rotation inside the grace window keeps only the immediately
		// previous secret: the first one stops verifying.
		previousRotated := rotated
		key = "rotate-source-again-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		status, body, _ = h.do(t, http.MethodPost, fmt.Sprintf("/api/v1/webhooks/inbound-sources/%d/rotate-secret", sourceA.ID), h.tokenA, nil, map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		rotatedAgain := decodeJSON[sourceDTO](t, body)
		require.NotEmpty(t, rotatedAgain.Secret)
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(rotated.Secret, "evt_in_previous"))
		assert.Equal(t, fiber.StatusAccepted, status, "the immediately previous secret must still verify")
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(sourceA.Secret, "evt_in_old_old"))
		assert.Equal(t, fiber.StatusUnauthorized, status, "an older secret must not survive a second rotation")
		rotated = rotatedAgain

		// After the explicit expiry only the current secret verifies.
		_, execErr := h.pool.Exec(ctx,
			`UPDATE webhook_inbound_sources SET previous_secret_expires_at = now() - interval '1 second' WHERE id = $1 AND tenant_id = $2`,
			sourceA.ID, h.tenantA)
		require.NoError(t, execErr)
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(previousRotated.Secret, "evt_in_expired"))
		assert.Equal(t, fiber.StatusUnauthorized, status)
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, headers(rotatedAgain.Secret, "evt_in_current"))
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

		// A captured request replayed under a fresh id no longer verifies:
		// the id is part of the signed content.
		replayed := headers(rotated.Secret, "evt_in_signed")
		replayed[webhook.HeaderID] = "evt_in_forged"
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, payload, replayed)
		assert.Equal(t, fiber.StatusUnauthorized, status)
		// Relabeling the event means editing the signed body, which also fails.
		relabeled := headers(rotated.Secret, "evt_in_relabel")
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL,
			[]byte(strings.Replace(string(payload), "invoice.paid", "invoice.refunded", 1)), relabeled)
		assert.Equal(t, fiber.StatusUnauthorized, status)
		// A signed body whose type is not a concrete event name is rejected.
		patternBody := []byte(`{"type":"invoice.*"}`)
		status, _, _ = h.doBytes(t, http.MethodPost, sourceA.ReceiverURL, patternBody,
			signFor(rotated.Secret, "evt_in_pattern", patternBody))
		assert.Equal(t, fiber.StatusBadRequest, status)

		// A suspended source tenant behaves like a missing source: the same
		// 401 as a bad signature, so ids cannot be enumerated.
		_, err := h.pool.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, h.tenantB)
		require.NoError(t, err)
		status, _, _ = h.doBytes(t, http.MethodPost, sourceB.ReceiverURL, payload, headers(sourceB.Secret, "evt_in_b_suspended"))
		assert.Equal(t, fiber.StatusUnauthorized, status)
		_, err = h.pool.Exec(ctx, `UPDATE tenants SET status = 'active' WHERE id = $1`, h.tenantB)
		require.NoError(t, err)
	})

	t.Run("transactional publish commits with the producer", func(t *testing.T) {
		endpoint := h.createEndpoint(t, h.tokenA, "https://receiver.example/tx", "tx.*")
		bus := events.New(jobs.NewQueue(testutil.Scoped(h.pool)))
		for _, eventType := range []string{"tx.rolled_back", "tx.committed"} {
			require.NoError(t, bus.SubscribeDurable(eventType, "webhooks", h.dispatcher.HandleEvent))
		}
		handlers := jobHandlers{}
		require.NoError(t, bus.RegisterJobs(handlers))

		countJobs := func() int {
			var n int
			require.NoError(t, h.pool.QueryRow(ctx,
				`SELECT count(*)::int FROM jobs WHERE tenant_id = $1 AND kind LIKE 'events.tx.%'`,
				h.tenantA,
			).Scan(&n))
			return n
		}
		before := countJobs()

		// A rolled-back producer transaction leaves no subscriber job behind.
		tx, err := h.pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, bus.PublishTx(ctx, tx, testEnvelope("", "tx.rolled_back", h.tenantA)))
		require.NoError(t, tx.Rollback(ctx))
		assert.Equal(t, before, countJobs())

		tx, err = h.pool.Begin(ctx)
		require.NoError(t, err)
		committed := testEnvelope("evt_tx_1", "tx.committed", h.tenantA)
		committed.Data = json.RawMessage(`{"n":1}`)
		require.NoError(t, bus.PublishTx(ctx, tx, committed))
		require.NoError(t, tx.Commit(ctx))
		require.Equal(t, before+1, countJobs())

		var payload []byte
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT payload FROM jobs WHERE tenant_id = $1 AND kind = 'events.tx.committed.webhooks'`,
			h.tenantA,
		).Scan(&payload))
		// At-least-once: running the subscriber job twice creates one delivery.
		for range 2 {
			require.NoError(t, handlers["events.tx.committed.webhooks"](ctx, payload))
		}
		var deliveries int
		var timeoutMS int32
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int, max(timeout_ms) FROM webhook_deliveries WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = 'evt_tx_1'`,
			h.tenantA, endpoint.ID,
		).Scan(&deliveries, &timeoutMS))
		assert.Equal(t, 1, deliveries)
		assert.Equal(t, int32(2000), timeoutMS, "the endpoint timeout is snapshotted on the delivery")
	})

	t.Run("unclaimable pending deliveries are dead-lettered", func(t *testing.T) {
		endpoint := h.createEndpoint(t, h.tokenA, "https://receiver.example/stuck", "stuck.*")
		tenantCtx := tenant.WithID(ctx, h.tenantA)
		for id, eventType := range map[string]string{"evt_stuck_exhausted": "stuck.one", "evt_stuck_disabled": "stuck.two"} {
			_, err := h.dispatcher.Dispatch(tenantCtx, testEnvelope(id, eventType, h.tenantA))
			require.NoError(t, err)
		}
		exhausted := h.deliveryID(t, "evt_stuck_exhausted")
		disabled := h.deliveryID(t, "evt_stuck_disabled")

		// Simulate a final attempt whose bookkeeping write was lost.
		_, err := h.pool.Exec(ctx,
			`UPDATE webhook_deliveries SET attempts = max_attempts, next_attempt_at = now() - interval '1 second' WHERE id = $1`,
			exhausted)
		require.NoError(t, err)
		job, _ := json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: exhausted})
		require.NoError(t, h.worker.Handle(tenantCtx, job))

		// A disabled endpoint stops receiving retries.
		_, err = h.pool.Exec(ctx, `UPDATE webhook_endpoints SET status = 'disabled' WHERE id = $1`, endpoint.ID)
		require.NoError(t, err)
		job, _ = json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: disabled})
		require.NoError(t, h.worker.Handle(tenantCtx, job))

		for id, reason := range map[int64]string{
			exhausted: "delivery attempts exhausted",
			disabled:  "endpoint is disabled or deleted",
		} {
			var status, lastError string
			var attempts int32
			require.NoError(t, h.pool.QueryRow(ctx,
				`SELECT status, COALESCE(last_error, ''), attempts FROM webhook_deliveries WHERE id = $1`, id,
			).Scan(&status, &lastError, &attempts))
			assert.Equal(t, "dead", status, "delivery %d", id)
			assert.Equal(t, reason, lastError, "delivery %d", id)
		}
		var disabledAttempts int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM webhook_delivery_attempts WHERE delivery_id = $1`, disabled,
		).Scan(&disabledAttempts))
		assert.Zero(t, disabledAttempts, "a disabled endpoint must not be called")
	})

	t.Run("concurrent secret rotations serialize", func(t *testing.T) {
		endpoint := h.createEndpoint(t, h.tokenA, "https://receiver.example/rotate", "*")
		tenantCtx := tenant.WithID(ctx, h.tenantA)
		const rotations = 8
		var wg sync.WaitGroup
		errs := make(chan error, rotations)
		for i := range rotations {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, err := h.repo.RotateEndpointSecret(tenantCtx, endpoint.ID, fmt.Sprintf("sealed-%d", i))
				errs <- err
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			assert.NoError(t, err)
		}
		var maxVersion, active int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT max(version), count(*) FILTER (WHERE active) FROM webhook_endpoint_secrets WHERE endpoint_id = $1`,
			endpoint.ID,
		).Scan(&maxVersion, &active))
		assert.Equal(t, rotations+1, maxVersion)
		assert.Equal(t, 1, active)
	})

	t.Run("subscriptions are a sorted set replaced on update", func(t *testing.T) {
		created := h.createEndpoint(t, h.tokenA, "https://receiver.example/subs", "file.uploaded", "account.*", "file.uploaded")
		assert.Equal(t, []string{"account.*", "file.uploaded"}, created.EventTypes)
		path := fmt.Sprintf("/api/v1/webhooks/endpoints/%d", created.ID)
		status, body, _ := h.do(t, http.MethodGet, path, h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		assert.Equal(t, []string{"account.*", "file.uploaded"}, decodeJSON[endpointDTO](t, body).EventTypes)

		status, body, _ = h.do(t, http.MethodPut, path, h.tokenA, map[string]any{
			"url": "https://receiver.example/subs", "status": "active", "eventTypes": []string{"user.*"},
		}, nil)
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		assert.Equal(t, []string{"user.*"}, decodeJSON[endpointDTO](t, body).EventTypes)
		status, body, _ = h.do(t, http.MethodGet, path, h.tokenA, nil, nil)
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
		assert.Equal(t, []string{"user.*"}, decodeJSON[endpointDTO](t, body).EventTypes)

		var rows int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM webhook_endpoint_subscriptions WHERE tenant_id = $1 AND endpoint_id = $2`,
			h.tenantA, created.ID).Scan(&rows))
		assert.Equal(t, 1, rows, "update replaces the subscription rows")

		active, err := h.repo.ActiveEndpoints(ctx, h.tenantA)
		require.NoError(t, err)
		for _, endpoint := range active {
			if endpoint.ID == created.ID {
				assert.Equal(t, []string{"user.*"}, endpoint.EventTypes)
				assert.True(t, webhookservice.Matches(endpoint.EventTypes, "user.registered"))
				assert.False(t, webhookservice.Matches(endpoint.EventTypes, "file.uploaded"))
			}
		}
	})

	t.Run("retention purges terminal rows and then unreferenced events in batches", func(t *testing.T) {
		endpoint := h.createEndpoint(t, h.tokenA, "https://receiver.example/purge", "purge.*")
		_, err := h.pool.Exec(ctx, `
			INSERT INTO webhook_events (tenant_id, event_id, event_type, data_version, actor_type, occurred_at, payload, created_at)
			SELECT $1::bigint, 'evt_purge_' || g, 'purge.old', 1, 'system', now(), '{}'::jsonb, now() - interval '40 days'
			FROM generate_series(1, 1500) AS g
			UNION ALL
			SELECT $1::bigint, 'evt_purge_pending', 'purge.old', 1, 'system', now(), '{}'::jsonb, now() - interval '40 days'
			UNION ALL
			SELECT $1::bigint, 'evt_purge_fresh', 'purge.old', 1, 'system', now(), '{}'::jsonb, now()`,
			h.tenantA)
		require.NoError(t, err)
		// Insert terminal rows already old: the updated_at trigger only fires on
		// UPDATE, and the app role must not need table ownership for DDL.
		_, err = h.pool.Exec(ctx, `
			INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_id,
			    url, secret_ciphertext, status, max_attempts, updated_at)
			SELECT $1, $2, 'evt_purge_' || g, 'https://receiver.example/purge',
			       'sealed', CASE WHEN g % 2 = 0 THEN 'succeeded' ELSE 'dead' END, 3,
			       now() - interval '40 days'
			FROM generate_series(1, 1500) AS g`,
			h.tenantA, endpoint.ID)
		require.NoError(t, err)
		_, err = h.pool.Exec(ctx, `
			INSERT INTO webhook_deliveries (tenant_id, endpoint_id, event_id,
			    url, secret_ciphertext, status, max_attempts)
			VALUES ($1, $2, 'evt_purge_pending', 'https://receiver.example/purge', 'sealed', 'pending', 3)`,
			h.tenantA, endpoint.ID)
		require.NoError(t, err)
		purged, err := h.repo.PurgeDeliveries(ctx, 30)
		require.NoError(t, err)
		assert.Equal(t, int64(1500), purged, "every terminal row is purged across batches")
		var pending int
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*)::int FROM webhook_deliveries WHERE event_id LIKE 'evt_purge_%'`,
		).Scan(&pending))
		assert.Equal(t, 1, pending, "pending deliveries are never purged")

		purgedEvents, err := h.repo.PurgeOrphanEvents(ctx, 30)
		require.NoError(t, err)
		assert.Equal(t, int64(1500), purgedEvents, "every unreferenced old event is purged across batches")
		var left []string
		rows, err := h.pool.Query(ctx,
			`SELECT event_id FROM webhook_events WHERE event_id LIKE 'evt_purge_%' ORDER BY event_id`)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			left = append(left, id)
		}
		require.NoError(t, rows.Err())
		assert.Equal(t, []string{"evt_purge_fresh", "evt_purge_pending"}, left,
			"a referenced event and an event younger than the window are kept")
	})
}

func (h *webhookHarness) doBytes(t *testing.T, method, path string, body []byte, headers map[string]string) (status int, payload []byte, header http.Header) {
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

// TestWebhooks_OutcomeWritesAreFencedByAttempt proves a stale worker whose
// claim lease expired cannot overwrite the newer attempt's result, and that an
// interrupted attempt is given back to the budget with its lease cleared.
func TestWebhooks_OutcomeWritesAreFencedByAttempt(t *testing.T) {
	h := setupWebhookHarness(t)
	ctx := tenant.WithID(context.Background(), h.tenantA)
	h.createEndpoint(t, h.tokenA, "http://127.0.0.1:9/hook", "fence.test")

	event := testEnvelope("evt_fence", "fence.test", h.tenantA)
	created, err := h.dispatcher.Dispatch(ctx, event)
	require.NoError(t, err)
	require.Equal(t, 1, created)
	id := h.deliveryID(t, event.ID)

	expireLease := func() {
		t.Helper()
		_, err := h.pool.Exec(ctx, `UPDATE webhook_deliveries SET next_attempt_at = NOW() - interval '1 second' WHERE id = $1`, id)
		require.NoError(t, err)
	}
	stale, err := h.repo.ClaimDelivery(ctx, id, time.Minute)
	require.NoError(t, err)
	require.Equal(t, int32(1), stale.Attempts)
	expireLease()
	current, err := h.repo.ClaimDelivery(ctx, id, time.Minute)
	require.NoError(t, err)
	require.Equal(t, int32(2), current.Attempts)

	// Every outcome write from the stale attempt is discarded.
	require.ErrorIs(t, h.repo.MarkDeliverySucceeded(ctx, id, stale.Attempts, http.StatusOK), webhookservice.ErrDeliveryLeaseLost)
	require.ErrorIs(t, h.repo.MarkDeliveryDead(ctx, id, stale.Attempts, http.StatusBadRequest, "stale"), webhookservice.ErrDeliveryLeaseLost)
	require.ErrorIs(t, h.repo.RescheduleDeliveryWithJob(ctx, webhookservice.RetryDraft{
		DeliveryID: id, Attempt: stale.Attempts, NextAttemptAt: time.Now().Add(time.Hour),
	}), webhookservice.ErrDeliveryLeaseLost)
	got, err := h.repo.GetDelivery(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "pending", got.Status)
	assert.Equal(t, int32(2), got.Attempts)

	// The current attempt is interrupted by shutdown: it is given back and
	// immediately claimable again with the same attempt number.
	require.NoError(t, h.repo.ReleaseDeliveryClaim(ctx, id, current.Attempts))
	require.ErrorIs(t, h.repo.ReleaseDeliveryClaim(ctx, id, current.Attempts), webhookservice.ErrDeliveryLeaseLost)
	reclaimed, err := h.repo.ClaimDelivery(ctx, id, time.Minute)
	require.NoError(t, err, "a released claim must be claimable without waiting for the lease")
	assert.Equal(t, int32(2), reclaimed.Attempts)

	// The current attempt's own result is applied.
	require.NoError(t, h.repo.MarkDeliverySucceeded(ctx, id, reclaimed.Attempts, http.StatusNoContent))
	got, err = h.repo.GetDelivery(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", got.Status)
}

// TestWebhooks_EndpointSecretRotationGraceRows proves rotation keeps exactly
// one previous secret with an explicit 24h expiry: a second rotation inside
// the window expires the older one instead of letting three secrets verify.
func TestWebhooks_EndpointSecretRotationGraceRows(t *testing.T) {
	h := setupWebhookHarness(t)
	ctx := context.Background()
	endpoint := h.createEndpoint(t, h.tokenA, "https://receiver.example/grace", "*")

	rotate := func() {
		t.Helper()
		key := "rotate-grace-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		status, body, _ := h.do(t, http.MethodPost,
			fmt.Sprintf("/api/v1/webhooks/endpoints/%d/rotate-secret", endpoint.ID), h.tokenA, nil,
			map[string]string{"Idempotency-Key": key})
		require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	}
	rotate()
	rotate()

	type secretRow struct {
		version   int32
		active    bool
		expiresAt *time.Time
	}
	rows, err := h.pool.Query(ctx,
		`SELECT version, active, grace_expires_at FROM webhook_endpoint_secrets
		 WHERE tenant_id = $1 AND endpoint_id = $2 ORDER BY version`,
		h.tenantA, endpoint.ID)
	require.NoError(t, err)
	defer rows.Close()
	var secrets []secretRow
	for rows.Next() {
		var row secretRow
		require.NoError(t, rows.Scan(&row.version, &row.active, &row.expiresAt))
		secrets = append(secrets, row)
	}
	require.NoError(t, rows.Err())
	require.Len(t, secrets, 3)

	assert.False(t, secrets[0].active)
	require.NotNil(t, secrets[0].expiresAt)
	assert.False(t, secrets[0].expiresAt.After(time.Now()),
		"a second rotation must expire the secret retired by the first one")

	assert.False(t, secrets[1].active)
	require.NotNil(t, secrets[1].expiresAt)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *secrets[1].expiresAt, time.Minute,
		"the immediately previous secret keeps the 24h grace window")

	assert.True(t, secrets[2].active)
	assert.Nil(t, secrets[2].expiresAt, "the active secret has no expiry")

	var activeCount, previousCount int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE active)::int,
		        count(*) FILTER (WHERE NOT active AND grace_expires_at > NOW())::int
		 FROM webhook_endpoint_secrets WHERE tenant_id = $1 AND endpoint_id = $2`,
		h.tenantA, endpoint.ID,
	).Scan(&activeCount, &previousCount))
	assert.Equal(t, 1, activeCount)
	assert.Equal(t, 1, previousCount, "at most one previous secret is valid at a time")
}

// TestWebhooks_EndpointUpdateRepointsPendingDeliveries proves an endpoint URL
// or timeout change applies to the endpoint's not-yet-terminal deliveries in
// the same transaction, so a retry cannot keep hitting the old receiver.
func TestWebhooks_EndpointUpdateRepointsPendingDeliveries(t *testing.T) {
	h := setupWebhookHarness(t)
	ctx := tenant.WithID(context.Background(), h.tenantA)
	oldURL := "https://receiver.example/old-hook"
	endpoint := h.createEndpoint(t, h.tokenA, oldURL, "move.event")

	pendingEvent := testEnvelope("evt_move_pending", "move.event", h.tenantA)
	doneEvent := testEnvelope("evt_move_done", "move.event", h.tenantA)
	for _, event := range []events.Event{pendingEvent, doneEvent} {
		created, err := h.dispatcher.Dispatch(ctx, event)
		require.NoError(t, err)
		require.Equal(t, 1, created)
	}
	pendingID := h.deliveryID(t, pendingEvent.ID)
	doneID := h.deliveryID(t, doneEvent.ID)
	_, err := h.pool.Exec(ctx,
		`UPDATE webhook_deliveries SET status = 'succeeded', delivered_at = now() WHERE id = $1`, doneID)
	require.NoError(t, err)

	newURL := "https://receiver.example/new-hook"
	status, body, _ := h.do(t, http.MethodPut,
		fmt.Sprintf("/api/v1/webhooks/endpoints/%d", endpoint.ID), h.tokenA, map[string]any{
			"url": newURL, "status": "active", "eventTypes": []string{"move.event"}, "timeoutMs": 5000,
		}, nil)
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)

	var pendingURL, doneURL string
	var pendingTimeout, doneTimeout int32
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT url, timeout_ms FROM webhook_deliveries WHERE id = $1`, pendingID,
	).Scan(&pendingURL, &pendingTimeout))
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT url, timeout_ms FROM webhook_deliveries WHERE id = $1`, doneID,
	).Scan(&doneURL, &doneTimeout))
	assert.Equal(t, newURL, pendingURL, "a pending delivery follows the endpoint URL change")
	assert.Equal(t, int32(5000), pendingTimeout, "a pending delivery follows the endpoint timeout change")
	assert.Equal(t, oldURL, doneURL, "terminal deliveries keep the snapshot they were sent with")
	assert.Equal(t, int32(2000), doneTimeout)
}

// TestWebhooks_ReplayKeepsOriginalWebhookID proves a replay is a new delivery
// of the original message: the receiver sees the original event id in
// webhook-id (and the body), so it can deduplicate it.
func TestWebhooks_ReplayKeepsOriginalWebhookID(t *testing.T) {
	h := setupWebhookHarness(t)
	ctx := tenant.WithID(context.Background(), h.tenantA)

	type received struct {
		id   string
		body map[string]any
	}
	var mu sync.Mutex
	var deliveries []received
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		deliveries = append(deliveries, received{id: r.Header.Get(webhook.HeaderID), body: parsed})
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	h.createEndpoint(t, h.tokenA, server.URL, "replay.event")
	event := testEnvelope("evt_replay_original", "replay.event", h.tenantA)
	created, err := h.dispatcher.Dispatch(ctx, event)
	require.NoError(t, err)
	require.Equal(t, 1, created)
	originalID := h.deliveryID(t, event.ID)

	run := func(deliveryID int64) {
		t.Helper()
		payload, err := json.Marshal(webhookservice.DeliveryJobPayload{DeliveryID: deliveryID})
		require.NoError(t, err)
		require.NoError(t, h.worker.Handle(ctx, payload))
	}
	run(originalID)

	status, body, _ := h.do(t, http.MethodPost,
		fmt.Sprintf("/api/v1/webhooks/deliveries/%d/replay", originalID), h.tokenA, nil, nil)
	require.Equal(t, fiber.StatusCreated, status, "body: %s", body)
	replayed := decodeJSON[deliveryDTO](t, body)
	assert.NotEqual(t, originalID, replayed.ID)
	assert.Equal(t, "evt_replay_original", replayed.EventID,
		"a replay references the original event so webhook-id stays stable")
	run(replayed.ID)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, deliveries, 2)
	for i, delivery := range deliveries {
		assert.Equal(t, "evt_replay_original", delivery.id, "delivery %d webhook-id", i)
		assert.Equal(t, "evt_replay_original", delivery.body["id"], "delivery %d body id", i)
	}
}

// testEnvelope is a minimal valid event envelope for dispatcher tests. An
// empty id is filled in by the bus on publish.
func testEnvelope(id, eventType string, tenantID int64) events.Event {
	return events.Event{
		ID:          id,
		Type:        eventType,
		DataVersion: 1,
		OccurredAt:  time.Now().UTC(),
		TenantID:    tenantID,
		Actor:       events.Actor{Type: events.ActorSystem},
		Data:        json.RawMessage(`{}`),
	}
}

// jobHandlers is a jobs.Registrar that keeps handlers for direct invocation.
type jobHandlers map[string]jobs.Handler

func (r jobHandlers) Register(kind string, handler jobs.Handler) { r[kind] = handler }
