// Command openapi writes the OpenAPI 3.1 document for the registered HTTP
// operations. It constructs the same module set as cmd/api but opens no
// database, cache, or network connection, so CI can verify the spec on every
// pull request.
//
//	go run ./cmd/openapi            # writes docs/openapi.yaml
//	go run ./cmd/openapi -out /tmp/openapi.yaml
package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/bootstrap"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil/sessiontest"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
)

func main() {
	out := flag.String("out", filepath.Join("docs", "openapi.yaml"), "OpenAPI 3.1 YAML output")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "openapi:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	cfg := generationConfig()
	modules, err := bootstrap.Build(cfg, app.Infra{
		Platform: app.NewPlatformConfig(cfg),
		Email:    jobs.NewQueuedSender(jobs.NewQueue(nil), nil),
	})
	if err != nil {
		return fmt.Errorf("build modules: %w", err)
	}
	application := app.New(cfg, app.Deps{
		Health:   health.NewChecker(nil, nil),
		Audit:    audit.Noop{},
		Sessions: sessiontest.AllowAll(),
	})
	application.Register(modules.List...)

	spec, err := application.Huma().OpenAPI().YAML()
	if err != nil {
		return fmt.Errorf("marshal OpenAPI: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	if err := os.WriteFile(out, spec, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}

// generationConfig is deliberately a literal: the generator must not depend on
// the operator's .env or a reachable database. Only module construction is
// exercised; no startup check reaches the network.
func generationConfig() *config.Config {
	return &config.Config{
		App: config.AppConfig{
			Env:            "test",
			BodyLimit:      12 << 20,
			JSONBodyLimit:  1 << 20,
			RequestTimeout: 12 * time.Second,
		},
		JWT: config.JWTConfig{
			Secret:     ephemeralKey(),
			AccessTTL:  15 * time.Minute,
			RefreshTTL: 7 * 24 * time.Hour,
		},
		DataEncryption: config.DataEncryptionConfig{Key: ephemeralKey()},
		Webhook:        config.WebhookConfig{SecretEncryptionKey: ephemeralKey()},
		CORS:           config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit:      config.RateLimitConfig{StrictMax: 5, StrictWindow: time.Minute, NormalMax: 60, NormalWindow: time.Minute, RelaxedMax: 120, RelaxedWindow: time.Minute},
		Tenant:         config.TenantConfig{DefaultSlug: "default"},
		APIKeys:        config.APIKeysConfig{MaxLifetime: 8760 * time.Hour},
		Files:          config.FilesConfig{ClamAVTimeout: 5 * time.Second},
	}
}

// ephemeralKey returns a fresh base64 32-byte key. Generation signs and seals
// nothing, so the keys only have to be valid, never stable or published.
func ephemeralKey() string {
	key := make([]byte, 32)
	_, _ = rand.Read(key) // crypto/rand.Read never returns an error (Go 1.24+).
	return base64.StdEncoding.EncodeToString(key)
}
