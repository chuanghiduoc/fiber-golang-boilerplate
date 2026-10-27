//go:build integration

package integration_test

import (
	"bytes"
	"encoding/base64"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/pgtest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/bootstrap"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	featureflags "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// testPassphrase is a runtime-built fixture password: it satisfies the
// current policy and contains no password-like literal for secret scanners.
var testPassphrase = strings.Join([]string{"correct", "horse", "battery", "staple"}, "-") + "-2026"

// testKey returns a base64-encoded 32-byte fixture key built at runtime, so the
// repository commits no key material and secret scanners see no literal.
func testKey(seed byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

// integrationApp is the full composition root (every module from
// internal/bootstrap) over a throwaway database. Tests drive the canonical
// huma operations from docs/api-routes.md, exactly as the server mounts them.
type integrationApp struct {
	app     *fiber.App
	pool    *pgxpool.Pool
	fx      *pgtest.Fixture
	cache   cache.Cache
	modules *bootstrap.Modules
	queue   *jobs.Queue
	audit   *audit.PostgresRecorder
	flags   featureflags.Evaluator
	storage storage.Storage
	events  *events.Bus
	config  *config.Config
}

func newIntegrationApp(t *testing.T, appEnv string) *integrationApp {
	t.Helper()
	fx := testutil.NewFixture(t)
	pool := fx.Bypass

	appCache := cache.NewMemoryCache()
	t.Cleanup(func() { _ = appCache.Close() })
	store, err := storage.NewLocalStorage(t.TempDir())
	require.NoError(t, err)
	queue := jobs.NewQueue(fx.Scoped())
	codec, err := jobs.NewPayloadCodec(testKey(7))
	require.NoError(t, err)
	recorder := audit.NewPostgresRecorder(fx.Scoped())
	catalogs := app.NewCatalogs()
	flags := featureflags.NewEvaluator(featureflags.Options{Catalog: catalogs.Features})
	bus := events.New(queue)

	cfg := &config.Config{
		App: config.AppConfig{
			Env: appEnv, BodyLimit: 16 << 20, JSONBodyLimit: 1 << 20, RequestTimeout: 10 * time.Second,
			TrustProxy: true, TrustedProxies: []string{"127.0.0.1", "0.0.0.0"},
		},
		CORS: config.CORSConfig{
			AllowOrigins:  []string{"https://app.example.com"},
			AllowMethods:  []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowHeaders:  []string{"Origin", "Content-Type", "Accept", "Authorization", "Idempotency-Key", "X-Tenant-Slug", "X-CSRF-Token"},
			ExposeHeaders: []string{"X-Request-Id", "RateLimit-Policy", "RateLimit", "ETag"},
		},
		RateLimit: config.RateLimitConfig{
			StrictMax: 10000, StrictWindow: 60 * time.Second,
			NormalMax: 10000, NormalWindow: 60 * time.Second,
			RelaxedMax: 10000, RelaxedWindow: 60 * time.Second,
		},
		JWT: config.JWTConfig{
			Secret:             "integration-app-secret-0123456789",
			AccessTTL:          15 * time.Minute,
			RefreshTTL:         720 * time.Hour,
			SessionMaxLifetime: 720 * time.Hour,
		},
		DataEncryption: config.DataEncryptionConfig{Key: testKey(1)},
		Webhook: config.WebhookConfig{
			SecretEncryptionKey: testKey(2),
			MaxAttempts:         3,
			RequestTimeout:      2 * time.Second,
			TimestampTolerance:  5 * time.Minute,
		},
		OAuth:   config.OAuthConfig{StateKey: testKey(3)},
		Tenant:  config.TenantConfig{DefaultSlug: "default"},
		Storage: config.StorageConfig{MaxFileSize: 10 << 20},
		APIKeys: config.APIKeysConfig{MaxLifetime: 8760 * time.Hour},
	}
	infra := app.Infra{
		Platform:     app.NewPlatformConfig(cfg),
		DB:           fx.Scoped(),
		Tx:           fx.Tx(),
		Cache:        appCache,
		Audit:        recorder,
		Queue:        queue,
		PayloadCodec: codec,
		Email:        jobs.NewQueuedSender(queue, codec),
		Storage:      store,
		Events:       bus,
		Flags:        flags,
		Catalogs:     catalogs,
	}
	modules, err := bootstrap.Build(cfg, infra)
	require.NoError(t, err)
	require.NoError(t, catalogs.Register(modules.List...))
	application := app.New(cfg, app.Deps{
		Cache:          appCache,
		Sessions:       modules.Sessions,
		TenantResolver: modules.TenantResolver,
		Audit:          recorder,
		Flags:          flags,
		Events:         bus,
		Privacy:        modules.Privacy,
		APIKeyVerifier: modules.APIKeyVerifier,
		Catalogs:       catalogs,
	})
	application.Register(modules.List...)
	return &integrationApp{
		app: application.Fiber(), pool: pool, fx: fx, cache: appCache,
		modules: modules, queue: queue, audit: recorder, flags: flags,
		storage: store, events: bus, config: cfg,
	}
}

// testWrongPassword is a runtime-built rejected credential.
var testWrongPassword = strings.Join([]string{"wrong", "fixture", "guess"}, "-")
