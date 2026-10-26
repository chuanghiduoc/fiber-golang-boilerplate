//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/bootstrap"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const privacyIntegrationSecret = "privacy-integration-secret-0123456789"

type privacyJobs struct {
	mu       sync.Mutex
	handlers map[string]jobs.Handler
}

func (r *privacyJobs) Register(kind string, handler jobs.Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = handler
}

func (r *privacyJobs) handler(t *testing.T, kind string) jobs.Handler {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	handler, ok := r.handlers[kind]
	require.True(t, ok, "no handler registered for %s", kind)
	return handler
}

type privacyHarness struct {
	app     *fiber.App
	pool    *pgxpool.Pool
	store   storage.Storage
	jobs    *privacyJobs
	modules *bootstrap.Modules
	records map[string]string
}

func setupPrivacyHarness(t *testing.T) *privacyHarness {
	t.Helper()
	ctx := context.Background()
	pool, cleanup, err := testutil.SetupTestDB(ctx)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	codec, err := jobs.NewPayloadCodec("privacy-integration-encryption-key")
	require.NoError(t, err)
	queue := jobs.NewQueue(testutil.Scoped(pool))
	bus := events.New(queue)
	recorder := audit.NewPostgresRecorder(testutil.Scoped(pool))
	flags := featureflags.NewEvaluator(featureflags.Options{})

	cfg := &config.Config{
		App:  config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS: config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{
			StrictMax: 10000, StrictWindow: 60,
			NormalMax: 10000, NormalWindow: 60,
			RelaxedMax: 10000, RelaxedWindow: 60,
		},
		JWT:    config.JWTConfig{Secret: privacyIntegrationSecret, ExpireHour: 1, RefreshExpireDays: 1},
		Tenant: config.TenantConfig{DefaultSlug: "default", RequireHeader: true},
	}
	infra := app.Infra{
		Platform:     app.NewPlatformConfig(cfg),
		Pool:         pool,
		DB:           testutil.Scoped(pool),
		Tx:           testutil.NewTxManager(pool),
		Cache:        appCache,
		Audit:        recorder,
		Queue:        queue,
		PayloadCodec: codec,
		Email:        jobs.NewQueuedSender(queue, jobs.WithPayloadCodec(codec)),
		Storage:      store,
		Events:       bus,
		Flags:        flags,
	}
	modules, err := bootstrap.Build(cfg, infra)
	require.NoError(t, err)

	registrar := &privacyJobs{handlers: map[string]jobs.Handler{}}
	application := app.New(cfg, app.Deps{
		Cache:          appCache,
		Sessions:       modules.Sessions,
		TenantResolver: modules.TenantResolver,
		Audit:          recorder,
		Flags:          flags,
		Events:         bus,
		Jobs:           registrar,
		Privacy:        modules.Privacy,
		APIKeyVerifier: modules.APIKeyVerifier,
	})
	application.Register(modules.List...)

	return &privacyHarness{app: application.Fiber(), pool: pool, store: store, jobs: registrar, modules: modules, records: map[string]string{}}
}

func (h *privacyHarness) request(t *testing.T, method, path, token, tenantSlug, body string) (status int, payload []byte) {
	t.Helper()
	return h.requestWithAuth(t, method, path, "Bearer "+token, tenantSlug, body)
}

func (h *privacyHarness) requestWithAuth(t *testing.T, method, path, authorization, tenantSlug, body string) (status int, payload []byte) {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, path, reader)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set(tenant.Header, tenantSlug)
	req.Header.Set("Idempotency-Key", strconv.FormatInt(time.Now().UnixNano(), 10))
	resp, err := h.app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	payload, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, payload
}

func (h *privacyHarness) runKind(t *testing.T, kind string) error {
	t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT tenant_id, payload FROM jobs WHERE kind = $1 ORDER BY id`, kind)
	require.NoError(t, err)
	defer rows.Close()
	handler := h.jobs.handler(t, kind)
	for rows.Next() {
		var tenantID int64
		var payload []byte
		require.NoError(t, rows.Scan(&tenantID, &payload))
		if err := handler(tenant.WithID(context.Background(), tenantID), payload); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (h *privacyHarness) seed(t *testing.T, tenantID, userID int64) {
	t.Helper()
	ctx := context.Background()
	storagePath := fmt.Sprintf("%d/%d/privacy-e2e.txt", tenantID, userID)
	require.NoError(t, h.store.Put(ctx, storagePath, strings.NewReader("hello"), 5, "text/plain"))
	_, err := h.pool.Exec(ctx,
		`INSERT INTO files (tenant_id, user_id, original_name, storage_path, mime_type, size)
		 VALUES ($1, $2, 'privacy-e2e.txt', $3, 'text/plain', 5)`,
		tenantID, userID, storagePath)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO notifications (tenant_id, user_id, type, title, body, data)
		 VALUES ($1, $2, 'security.alert', 'Privacy', 'Body', '{}'::jsonb)`,
		tenantID, userID)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO notification_preferences (tenant_id, user_id, type, channel, enabled)
		 VALUES ($1, $2, 'security.alert', 'email', false)`,
		tenantID, userID)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash)
		 VALUES ($1, $2, 'privacy-key', 'priv', $3)`,
		tenantID, userID, fmt.Sprintf("hash-%d-%d", tenantID, userID))
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO feature_flags (flag_key, enabled) VALUES ('privacy_test_flag', true)
		 ON CONFLICT (flag_key) DO NOTHING`)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO feature_flag_targets (flag_key, tenant_id, user_id, enabled)
		 VALUES ('privacy_test_flag', $1, $2, true)`,
		tenantID, userID)
	require.NoError(t, err)

	var endpointID int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO webhook_endpoints (tenant_id, url) VALUES ($1, 'https://example.test/hook') RETURNING id`,
		tenantID).Scan(&endpointID))
	eventID := fmt.Sprintf("evt-%d-%d", tenantID, userID)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO webhook_events
		    (tenant_id, event_id, event_type, data_version, actor_type, actor_id, occurred_at, payload)
		 VALUES ($1, $2, 'user.registered', 1, 'user', $3, NOW(),
		         jsonb_build_object('userId', $4::bigint, 'email', $5::text))`,
		tenantID, eventID, strconv.FormatInt(userID, 10), userID, fmt.Sprintf("privacy-%d-%d@example.com", tenantID, userID))
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx,
		`INSERT INTO webhook_deliveries
		    (tenant_id, endpoint_id, event_id, url, secret_ciphertext, status, max_attempts, timeout_ms, delivered_at)
		 VALUES ($1, $2, $3, 'https://example.test/hook', 'sealed', 'succeeded', 8, 10000, NOW())`,
		tenantID, endpointID, eventID)
	require.NoError(t, err)
	h.records[fmt.Sprintf("%d/%d", tenantID, userID)] = storagePath
}

func decodeExport(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		Sections map[string]json.RawMessage `json:"sections"`
	}
	require.NoError(t, json.Unmarshal(body, &doc))
	return doc.Sections
}

func TestPrivacy_ExportEndToEndAndIsolation(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	tenantB := testutil.CreateTenant(t, ctx, h.pool, "privacy-b")
	user := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-export@example.com", testutil.MemberOptions{Password: "Password1!"})
	testutil.CreateMember(t, ctx, h.pool, tenantB, "privacy-export@example.com", testutil.MemberOptions{})
	other := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-other@example.com", testutil.MemberOptions{})
	h.seed(t, tenant.DefaultID, user.UserID)
	h.seed(t, tenantB, user.UserID)

	token := testutil.SessionToken(t, ctx, h.pool, user, privacyIntegrationSecret)
	otherToken := testutil.SessionToken(t, ctx, h.pool, other, privacyIntegrationSecret)
	tenantBToken := testutil.SessionToken(t, ctx, h.pool, testutil.Member{UserID: user.UserID, TenantID: tenantB, Email: user.Email, Role: "user"}, privacyIntegrationSecret)

	status, body := h.request(t, http.MethodPost, "/api/v1/me/data-exports", token, "default", "")
	require.Equal(t, fiber.StatusCreated, status, "body: %s", body)
	var created struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	assert.Equal(t, "queued", created.Status)

	// A second active request is rejected by the partial unique index.
	status, body = h.request(t, http.MethodPost, "/api/v1/me/data-exports", token, "default", "")
	assert.Equal(t, fiber.StatusConflict, status, "body: %s", body)

	require.NoError(t, h.runKind(t, "privacy.data_export"))

	status, body = h.request(t, http.MethodGet,
		"/api/v1/me/data-exports/"+strconv.FormatInt(created.ID, 10), token, "default", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	var ready struct {
		Status      string `json:"status"`
		DownloadURL string `json:"downloadUrl"`
	}
	require.NoError(t, json.Unmarshal(body, &ready))
	assert.Equal(t, "ready", ready.Status)
	assert.NotEmpty(t, ready.DownloadURL)

	// Same user in another tenant and another user in the same tenant get 404.
	status, _ = h.request(t, http.MethodGet,
		"/api/v1/me/data-exports/"+strconv.FormatInt(created.ID, 10), tenantBToken, "privacy-b", "")
	assert.Equal(t, fiber.StatusNotFound, status)
	status, _ = h.request(t, http.MethodGet,
		"/api/v1/me/data-exports/"+strconv.FormatInt(created.ID, 10), otherToken, "default", "")
	assert.Equal(t, fiber.StatusNotFound, status)
	status, _ = h.request(t, http.MethodGet,
		"/api/v1/me/data-exports/"+strconv.FormatInt(created.ID, 10)+"/download", tenantBToken, "privacy-b", "")
	assert.Equal(t, fiber.StatusNotFound, status)

	status, body = h.request(t, http.MethodGet,
		"/api/v1/me/data-exports/"+strconv.FormatInt(created.ID, 10)+"/download", token, "default", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	sections := decodeExport(t, body)
	for _, name := range []string{"identity", "files", "notifications", "apikeys", "features", "webhooks"} {
		assert.Contains(t, sections, name)
	}
	var webhookSection struct {
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	require.NoError(t, json.Unmarshal(sections["webhooks"], &webhookSection))
	require.Len(t, webhookSection.Events, 1)
	assert.Equal(t, "user.registered", webhookSection.Events[0].Type)
	var fileSection struct {
		Files []struct {
			Name          string `json:"name"`
			ContentBase64 string `json:"contentBase64"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(sections["files"], &fileSection))
	require.Len(t, fileSection.Files, 1)
	content, err := base64.StdEncoding.DecodeString(fileSection.Files[0].ContentBase64)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))

	// The tenant-B export row was never created.
	var tenantBRows int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM data_exports WHERE tenant_id = $1`, tenantB).Scan(&tenantBRows))
	assert.Zero(t, tenantBRows)
}

func TestPrivacy_ErasureIsIdempotentAndAnonymizes(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	tenantB := testutil.CreateTenant(t, ctx, h.pool, "privacy-erase-b")
	user := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-erase@example.com", testutil.MemberOptions{Password: "Password1!"})
	testutil.CreateMember(t, ctx, h.pool, tenantB, "privacy-erase@example.com", testutil.MemberOptions{})
	h.seed(t, tenant.DefaultID, user.UserID)
	h.seed(t, tenantB, user.UserID)
	// A confirmed TOTP factor and recovery codes: the user row is anonymized,
	// not deleted, so the cascade cannot remove them.
	_, err := h.pool.Exec(ctx, `INSERT INTO mfa_factors (user_id, type, secret_ciphertext, confirmed_at)
		VALUES ($1, 'totp', 'sealed', now())`, user.UserID)
	require.NoError(t, err)
	_, err = h.pool.Exec(ctx, `INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES ($1, 'h1'), ($1, 'h2')`, user.UserID)
	require.NoError(t, err)
	token := testutil.SessionToken(t, ctx, h.pool, user, privacyIntegrationSecret)
	bToken := testutil.SessionToken(t, ctx, h.pool,
		testutil.Member{UserID: user.UserID, TenantID: tenantB, Email: user.Email, Role: "user"}, privacyIntegrationSecret)

	// A ready export exists when erasure is requested; its document must be
	// deleted, not just hidden from the list.
	status, body := h.request(t, http.MethodPost, "/api/v1/me/data-exports", token, "default", "")
	require.Equal(t, fiber.StatusCreated, status, "body: %s", body)
	var export struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &export))
	require.NoError(t, h.runKind(t, "privacy.data_export"))
	var exportPath string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT storage_path FROM data_exports WHERE id = $1`, export.ID).Scan(&exportPath))
	require.NotEmpty(t, exportPath)

	// Wrong password re-authentication is refused before anything is queued.
	status, body = h.request(t, http.MethodPost, "/api/v1/me/erasure", token, "default", `{"currentPassword":"wrong"}`)
	assert.Equal(t, fiber.StatusBadRequest, status, "body: %s", body)
	var queued int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind IN ('privacy.erase', 'privacy.erase.finalize')`).Scan(&queued))
	assert.Zero(t, queued)

	status, body = h.request(t, http.MethodPost, "/api/v1/me/erasure", token, "default", `{"currentPassword":"Password1!"}`)
	require.Equal(t, fiber.StatusAccepted, status, "body: %s", body)

	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'privacy.erase'`).Scan(&queued))
	assert.Equal(t, 2, queued, "one erase job per membership")
	var finalizers int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'privacy.erase.finalize'`).Scan(&finalizers))
	assert.Equal(t, 1, finalizers)

	var metadata string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT metadata::text FROM audit_events WHERE action = 'user.erasure_requested' ORDER BY id DESC LIMIT 1`).Scan(&metadata))
	assert.NotContains(t, metadata, user.Email)

	require.NoError(t, h.runKind(t, "privacy.erase"))
	require.NoError(t, h.runKind(t, "privacy.erase.finalize"))

	assertUserErased(t, h, tenantB, user.UserID)
	// At-least-once redelivery is a no-op.
	require.NoError(t, h.runKind(t, "privacy.erase"))
	require.NoError(t, h.runKind(t, "privacy.erase.finalize"))

	for _, tenantID := range []int64{tenant.DefaultID, tenantB} {
		var files, notifications, preferences, keys, targets, webhookEvents, webhookDeliveries int
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE tenant_id = $1 AND user_id = $2`, tenantID, user.UserID).Scan(&files))
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE tenant_id = $1 AND user_id = $2`, tenantID, user.UserID).Scan(&notifications))
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM notification_preferences WHERE tenant_id = $1 AND user_id = $2`, tenantID, user.UserID).Scan(&preferences))
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE tenant_id = $1 AND user_id = $2`, tenantID, user.UserID).Scan(&keys))
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM feature_flag_targets WHERE tenant_id = $1 AND user_id = $2`, tenantID, user.UserID).Scan(&targets))
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM webhook_events WHERE tenant_id = $1 AND actor_id = $2`, tenantID, strconv.FormatInt(user.UserID, 10)).Scan(&webhookEvents))
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT count(*) FROM webhook_deliveries d JOIN webhook_events e
			   ON e.tenant_id = d.tenant_id AND e.event_id = d.event_id
			 WHERE d.tenant_id = $1 AND e.actor_id = $2`, tenantID, strconv.FormatInt(user.UserID, 10)).Scan(&webhookDeliveries))
		assert.Zero(t, files+notifications+preferences+keys+targets+webhookEvents+webhookDeliveries,
			"tenant %d still holds per-user rows", tenantID)
		_, err := h.store.Get(ctx, h.records[fmt.Sprintf("%d/%d", tenantID, user.UserID)])
		assert.ErrorIs(t, err, storage.ErrNotFound)
	}
	var exports int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM data_exports WHERE user_id = $1`, user.UserID).Scan(&exports))
	assert.Zero(t, exports)
	_, exportErr := h.store.Get(ctx, exportPath)
	assert.ErrorIs(t, exportErr, storage.ErrNotFound, "erasure must delete the ready export document")

	// The erased tokens cannot read the account anymore.
	status, _ = h.request(t, http.MethodGet, "/api/v1/me/data-exports", token, "default", "")
	assert.Equal(t, fiber.StatusUnauthorized, status)
	status, _ = h.request(t, http.MethodGet, "/api/v1/me/data-exports", bToken, "privacy-erase-b", "")
	assert.Equal(t, fiber.StatusUnauthorized, status)
}

func assertUserErased(t *testing.T, h *privacyHarness, tenantB, userID int64) {
	t.Helper()
	ctx := context.Background()
	var email, name, status string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT email, name, status FROM users WHERE id = $1`, userID).Scan(&email, &name, &status))
	assert.Equal(t, fmt.Sprintf("erased+%d@invalid", userID), email)
	assert.Empty(t, name)
	assert.Equal(t, "deleted", status)

	for _, table := range []string{"user_passwords", "user_identities", "sessions", "password_reset_tokens", "email_verification_tokens", "mfa_factors", "mfa_recovery_codes"} {
		var count int
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE user_id = $1`, userID).Scan(&count))
		assert.Zero(t, count, table)
	}
	for _, tenantID := range []int64{tenant.DefaultID, tenantB} {
		var membershipStatus string
		require.NoError(t, h.pool.QueryRow(ctx,
			`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID).Scan(&membershipStatus))
		assert.Equal(t, "removed", membershipStatus)
	}
}

// An admin erasure is scoped to the request tenant: it removes that
// membership and that tenant's data, but never the global account or the
// user's other memberships.
func TestPrivacy_AdminErasureStaysInTheRequestTenant(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	tenantB := testutil.CreateTenant(t, ctx, h.pool, "privacy-admin-b")
	admin := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-admin@example.com", testutil.MemberOptions{Role: "admin"})
	user := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-admin-target@example.com", testutil.MemberOptions{Password: "Password1!"})
	testutil.CreateMember(t, ctx, h.pool, tenantB, "privacy-admin-target@example.com", testutil.MemberOptions{})
	h.seed(t, tenant.DefaultID, user.UserID)
	h.seed(t, tenantB, user.UserID)
	token := testutil.SessionToken(t, ctx, h.pool, admin, privacyIntegrationSecret)

	// A user who is only a member of another tenant is a 404, not a hint that
	// they exist (RLS keeps the lookup scoped even though this route is
	// tenant-aware rather than global).
	outsider := testutil.CreateMember(t, ctx, h.pool, tenantB, "privacy-admin-outsider@example.com", testutil.MemberOptions{})
	status, body := h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(outsider.UserID, 10)+"/erasure", token, "default", "")
	require.Equal(t, fiber.StatusNotFound, status, "body: %s", body)

	status, body = h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(user.UserID, 10)+"/erasure", token, "default", "")
	require.Equal(t, fiber.StatusAccepted, status, "body: %s", body)

	var eraseJobs, finalizers int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'privacy.erase' AND payload->>'userId' = $1`,
		strconv.FormatInt(user.UserID, 10)).Scan(&eraseJobs))
	assert.Equal(t, 1, eraseJobs, "exactly one erase job for the request tenant")
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind = 'privacy.erase.finalize'`).Scan(&finalizers))
	assert.Zero(t, finalizers, "admin erasure must not enqueue the global finalizer")

	require.NoError(t, h.runKind(t, "privacy.erase"))

	var statusA, statusB string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant.DefaultID, user.UserID).Scan(&statusA))
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantB, user.UserID).Scan(&statusB))
	assert.Equal(t, "removed", statusA)
	assert.Equal(t, "active", statusB, "the other tenant's membership must stay intact")

	var email, name, userStatus string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT email, name, status FROM users WHERE id = $1`, user.UserID).Scan(&email, &name, &userStatus))
	assert.Equal(t, user.Email, email, "the global user must not be anonymized")
	assert.NotEmpty(t, name)
	assert.Equal(t, "active", userStatus)

	var tenantBFiles int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE tenant_id = $1 AND user_id = $2`, tenantB, user.UserID).Scan(&tenantBFiles))
	assert.Equal(t, 1, tenantBFiles, "the other tenant's per-user data stays")
	var tenantAFiles int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE tenant_id = $1 AND user_id = $2`, tenant.DefaultID, user.UserID).Scan(&tenantAFiles))
	assert.Zero(t, tenantAFiles)
}

// A ban recorded before the hash column existed (or by a direct SQL repair)
// still keeps the address blocked: removal derives the missing hash from the
// user's current normalized email.
func TestPrivacy_AdminRemovalBackfillsAMissingBanHash(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	admin := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-backfill-admin@example.com", testutil.MemberOptions{Role: "admin"})
	user := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-backfill@example.com", testutil.MemberOptions{})
	_, err := h.pool.Exec(ctx,
		`UPDATE memberships SET status = 'suspended', banned_at = NOW(), credentials_revoked_at = NOW()
		 WHERE tenant_id = $1 AND user_id = $2`, tenant.DefaultID, user.UserID)
	require.NoError(t, err)
	token := testutil.SessionToken(t, ctx, h.pool, admin, privacyIntegrationSecret)

	status, body := h.request(t, http.MethodDelete,
		"/api/v1/users/"+strconv.FormatInt(user.UserID, 10), token, "default", "")
	require.Equal(t, fiber.StatusNoContent, status, "body: %s", body)

	var membershipStatus string
	var hash *string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT status, banned_email_hash FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenant.DefaultID, user.UserID).Scan(&membershipStatus, &hash))
	assert.Equal(t, "removed", membershipStatus)
	require.NotNil(t, hash, "removal must derive the marker for a legacy ban")
	sum := sha256.Sum256([]byte(identityservice.NormalizeEmail(user.Email)))
	assert.Equal(t, hex.EncodeToString(sum[:]), *hash)
}

// A tenant ban survives the banned user's self-erasure, so the same address
// cannot register into that tenant again even though the global account was
// anonymized (GDPR Art.17(3)(e): the pseudonymous hash defends the ban).
func TestPrivacy_BanSurvivesSelfErasure(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	tenantB := testutil.CreateTenant(t, ctx, h.pool, "privacy-ban-b")
	admin := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-ban-admin@example.com", testutil.MemberOptions{Role: "admin"})
	user := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-ban@example.com", testutil.MemberOptions{Password: "Password1!"})
	memberB := testutil.CreateMember(t, ctx, h.pool, tenantB, "privacy-ban@example.com", testutil.MemberOptions{})
	adminToken := testutil.SessionToken(t, ctx, h.pool, admin, privacyIntegrationSecret)

	status, body := h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(user.UserID, 10)+"/ban", adminToken, "default", "")
	require.Equal(t, fiber.StatusNoContent, status, "body: %s", body)

	var bannedAt *time.Time
	var hash string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT banned_at, banned_email_hash FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenant.DefaultID, user.UserID).Scan(&bannedAt, &hash))
	require.NotNil(t, bannedAt)
	require.Len(t, hash, 64)
	sum := sha256.Sum256([]byte(identityservice.NormalizeEmail(user.Email)))
	assert.Equal(t, hex.EncodeToString(sum[:]), hash, "SQL and Go must use the same normalization")

	// The user erases the global account from the tenant where they are not
	// banned; the banned membership must keep its marker through the removal.
	bToken := testutil.SessionToken(t, ctx, h.pool, memberB, privacyIntegrationSecret)
	status, body = h.request(t, http.MethodPost, "/api/v1/me/erasure", bToken, "privacy-ban-b",
		`{"currentPassword":"Password1!"}`)
	require.Equal(t, fiber.StatusAccepted, status, "body: %s", body)
	require.NoError(t, h.runKind(t, "privacy.erase"))
	require.NoError(t, h.runKind(t, "privacy.erase.finalize"))

	var membershipStatus string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT status, banned_at, banned_email_hash FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenant.DefaultID, user.UserID).Scan(&membershipStatus, &bannedAt, &hash))
	assert.Equal(t, "removed", membershipStatus)
	assert.NotNil(t, bannedAt, "the ban timestamp survives erasure")
	assert.Equal(t, hex.EncodeToString(sum[:]), hash, "the pseudonymous marker survives erasure")
	assertUserErased(t, h, tenantB, user.UserID)

	// The anonymized account's old address cannot register into the tenant
	// that banned it, but it is free in the other tenant.
	status, body = h.requestWithAuth(t, http.MethodPost, "/api/v1/auth/register", "", "default",
		`{"email":"privacy-ban@example.com","password":"Password1!","name":"Again"}`)
	assert.Equal(t, fiber.StatusConflict, status, "body: %s", body)
	status, body = h.requestWithAuth(t, http.MethodPost, "/api/v1/auth/register", "", "privacy-ban-b",
		`{"email":"privacy-ban@example.com","password":"Password1!","name":"Again"}`)
	assert.Equal(t, fiber.StatusCreated, status, "body: %s", body)
}

func TestPrivacy_UnbanClearsThePseudonymousBanMarker(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	admin := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-unban-admin@example.com", testutil.MemberOptions{Role: "admin"})
	user := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-unban@example.com", testutil.MemberOptions{})
	token := testutil.SessionToken(t, ctx, h.pool, admin, privacyIntegrationSecret)
	path := "/api/v1/admin/users/" + strconv.FormatInt(user.UserID, 10)

	status, body := h.request(t, http.MethodPost, path+"/ban", token, "default", "")
	require.Equal(t, fiber.StatusNoContent, status, "body: %s", body)
	var bannedAt, hash *string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT banned_at::text, banned_email_hash FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenant.DefaultID, user.UserID).Scan(&bannedAt, &hash))
	require.NotNil(t, bannedAt)
	require.NotNil(t, hash)

	status, body = h.request(t, http.MethodPost, path+"/unban", token, "default", "")
	require.Equal(t, fiber.StatusOK, status, "body: %s", body)
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT banned_at::text, banned_email_hash FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenant.DefaultID, user.UserID).Scan(&bannedAt, &hash))
	assert.Nil(t, bannedAt)
	assert.Nil(t, hash)
}

func TestPrivacy_BannedEmailHashMatchesGoNormalization(t *testing.T) {
	h := setupPrivacyHarness(t)
	inputs := []string{"  Alice@Example.COM  ", "BOB+tag@Sub.Example.TEST", "carol@example.test"}
	for _, in := range inputs {
		var sqlHash string
		require.NoError(t, h.pool.QueryRow(context.Background(),
			`SELECT encode(sha256(convert_to(lower(btrim($1)), 'UTF8')), 'hex')`, in).Scan(&sqlHash))
		sum := sha256.Sum256([]byte(identityservice.NormalizeEmail(in)))
		assert.Equal(t, hex.EncodeToString(sum[:]), sqlHash, "input %q", in)
	}
}

func TestPrivacy_AdminErasureRejectsAnAPIKey(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	admin := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-key-admin@example.com", testutil.MemberOptions{Role: "admin"})
	target := testutil.CreateMember(t, ctx, h.pool, tenant.DefaultID, "privacy-key-target@example.com", testutil.MemberOptions{})
	creds, err := apikey.Generate()
	require.NoError(t, err)
	var keyID int64
	require.NoError(t, h.pool.QueryRow(ctx,
		`INSERT INTO api_keys (tenant_id, user_id, name, prefix, key_hash)
		 VALUES ($1, $2, 'privacy-admin-key', $3, $4) RETURNING id`,
		tenant.DefaultID, admin.UserID, creds.Prefix, creds.Hash).Scan(&keyID))
	_, err = h.pool.Exec(ctx,
		`INSERT INTO api_key_scopes (api_key_id, permission_key) VALUES ($1, 'admin:users:delete')`, keyID)
	require.NoError(t, err)

	status, body := h.requestWithAuth(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(target.UserID, 10)+"/erasure",
		"ApiKey "+creds.Key, "default", "")
	assert.Equal(t, fiber.StatusUnauthorized, status, "body: %s", body)
	var queued int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind LIKE 'privacy.%' AND payload->>'userId' = $1`,
		strconv.FormatInt(target.UserID, 10)).Scan(&queued))
	assert.Zero(t, queued)
}

func TestPrivacy_LastOwnerErasureIsRefused(t *testing.T) {
	h := setupPrivacyHarness(t)
	ctx := context.Background()
	tenantC := testutil.CreateTenant(t, ctx, h.pool, "privacy-owner-c")
	owner := testutil.CreateMember(t, ctx, h.pool, tenantC, "privacy-owner@example.com", testutil.MemberOptions{Role: "owner"})
	token := testutil.SessionToken(t, ctx, h.pool, owner, privacyIntegrationSecret)

	status, body := h.request(t, http.MethodPost,
		"/api/v1/admin/users/"+strconv.FormatInt(owner.UserID, 10)+"/erasure", token, "privacy-owner-c", "")
	assert.Equal(t, fiber.StatusConflict, status, "body: %s", body)
	var jobsForUser int
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobs WHERE kind LIKE 'privacy.%' AND payload->>'userId' = $1`,
		strconv.FormatInt(owner.UserID, 10)).Scan(&jobsForUser))
	assert.Zero(t, jobsForUser, "nothing may be queued before the last-owner refusal")

	var membershipStatus string
	require.NoError(t, h.pool.QueryRow(ctx,
		`SELECT status FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenantC, owner.UserID).Scan(&membershipStatus))
	assert.Equal(t, "active", membershipStatus)
}
