package app_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// countingKeyStore records every API-key lookup and never finds a key, like a
// flood of bogus credentials against the real store.
type countingKeyStore struct{ lookups atomic.Int32 }

func (s *countingKeyStore) FindByHash(context.Context, string) (apikey.Key, error) {
	s.lookups.Add(1)
	return apikey.Key{}, apperror.ErrNotFound
}

func (*countingKeyStore) TouchLastUsed(context.Context, int64, time.Time) error { return nil }

type protectedModule struct{}

func (protectedModule) Name() string { return "protected" }
func (protectedModule) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	group := v1.Group("/protected", deps.Authenticate(), deps.Limiters.Normal)
	group.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })
}

func TestAuthenticateLimitsPerIPBeforeVerifying(t *testing.T) {
	const authMax = 3
	store := &countingKeyStore{}
	cfg := &config.Config{
		App:  config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5},
		CORS: config.CORSConfig{AllowOrigins: "*"},
		RateLimit: config.RateLimitConfig{
			StrictMax: 5, StrictWindow: 60, NormalMax: 60, NormalWindow: 60, RelaxedMax: 120, RelaxedWindow: 60,
			// A long window keeps a boundary crossing out of this short test.
			AuthMax: authMax, AuthWindow: 3600,
		},
		Tenant: config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{APIKeyVerifier: apikey.NewVerifier(store, nil)})
	application.Register(protectedModule{})

	creds, err := apikey.Generate()
	require.NoError(t, err)

	statuses := make([]int, 0, authMax+5)
	for range authMax + 5 {
		req, _ := http.NewRequest(http.MethodGet, "/api/v1/protected", http.NoBody)
		req.Header.Set(fiber.HeaderAuthorization, "ApiKey "+creds.Key)
		resp, err := application.Fiber().Test(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		statuses = append(statuses, resp.StatusCode)
	}

	for i, status := range statuses {
		if i < authMax {
			assert.Equal(t, fiber.StatusUnauthorized, status, "request %d should reach the verifier", i)
		} else {
			assert.Equal(t, fiber.StatusTooManyRequests, status, "request %d should be throttled", i)
		}
	}
	assert.Equal(t, int32(authMax), store.lookups.Load(),
		"throttled requests must be rejected before the API-key store is queried")
}
