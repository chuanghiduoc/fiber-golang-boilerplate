package arch_test

import (
	"context"
	"encoding/base64"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/bootstrap"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
)

const archTestSecret = "arch-route-coverage-secret-0123456789"

var (
	archDataKey    = archKey(1)
	archWebhookKey = archKey(2)
)

// modulePath is the Go module path used by the import-boundary checks.
const modulePath = "github.com/chuanghiduoc/fiber-golang-boilerplate"

// productionBootstrap builds the same ordered module set cmd/api uses
// (internal/bootstrap). Tests that inspect composition-root wiring (the
// tenancy registry) use this directly.
func productionBootstrap(t *testing.T) (*bootstrap.Modules, app.Catalogs) {
	t.Helper()
	catalogs := app.NewCatalogs()
	modules, err := bootstrap.Build(archConfig(), app.Infra{
		Platform: app.NewPlatformConfig(archConfig()),
		Email:    jobs.NewQueuedSender(jobs.NewQueue(nil), nil),
		Catalogs: catalogs,
	})
	if err != nil {
		t.Fatalf("bootstrap.Build: %v", err)
	}
	if err := catalogs.Register(modules.List...); err != nil {
		t.Fatalf("RegisterCatalogs: %v", err)
	}
	return modules, catalogs
}

// productionModules builds every module from the same ordered list cmd/api
// uses (internal/bootstrap). Construction performs no I/O, so nil
// infrastructure is enough to mount and inspect the operation tables.
func productionModules(t *testing.T) ([]app.Module, app.Deps) {
	t.Helper()
	modules, catalogs := productionBootstrap(t)
	return modules.List, archDeps(catalogs)
}

func archConfig() *config.Config {
	return &config.Config{
		App:            config.AppConfig{Env: "test", BodyLimit: 1 << 20, RequestTimeout: 12 * time.Second},
		CORS:           config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit:      config.RateLimitConfig{StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second},
		JWT:            config.JWTConfig{Secret: archTestSecret, AccessTTL: 15 * time.Minute, RefreshTTL: time.Hour},
		DataEncryption: config.DataEncryptionConfig{Key: archDataKey},
		Webhook:        config.WebhookConfig{SecretEncryptionKey: archWebhookKey},
		Tenant:         config.TenantConfig{DefaultSlug: "default"},
	}
}

func archDeps(catalogs app.Catalogs) app.Deps {
	cfg := archConfig()
	return app.Deps{
		Config:   app.ModuleSettings{JWT: cfg.JWT, MFA: cfg.MFA, CORS: cfg.CORS, Tenant: cfg.Tenant},
		Audit:    audit.Noop{},
		Sessions: archSessions{},
		Catalogs: catalogs,
	}
}

// archOperations returns the operations of the REAL module set built from
// internal/bootstrap. There is no stub fixture: every rule is enforced against
// the same operations the server mounts.
func archOperations(t *testing.T) []httpapi.OperationInfo {
	t.Helper()
	modules, deps := productionModules(t)
	application := app.New(archConfig(), deps)
	application.Register(modules...)
	return application.HTTP().Operations()
}

// archSessions is a permissive session store: arch tests inspect route
// registration, not session validation, but JWT authentication always needs
// a wired store.
type archSessions struct{}

func (archSessions) Validate(context.Context, session.Claims) (session.State, error) {
	return session.State{Active: true, TenantActive: true}, nil
}

func (archSessions) Invalidate(context.Context, uuid.UUID) error { return nil }
func (archSessions) InvalidatePermissions(context.Context) error { return nil }

// archKey builds base64 key material at runtime; tests commit no key values.
func archKey(seed byte) string {
	buf := make([]byte, 32)
	for i := range buf {
		buf[i] = seed
	}
	return base64.StdEncoding.EncodeToString(buf)
}
