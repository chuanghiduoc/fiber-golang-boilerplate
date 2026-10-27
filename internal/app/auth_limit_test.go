package app_test

import (
	"context"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// countingKeyStore records every API-key lookup and never finds a key, like a
// flood of bogus credentials against the real store.
type countingKeyStore struct{ lookups atomic.Int32 }

func (s *countingKeyStore) FindByHash(context.Context, []byte) (apikey.Key, error) {
	s.lookups.Add(1)
	return apikey.Key{}, apperror.ErrNotFound
}

func (*countingKeyStore) TouchLastUsed(context.Context, uuid.UUID, time.Time) error { return nil }

type protectedModule struct{}

func (protectedModule) Name() string { return "protected" }
func (protectedModule) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	httpapi.Register(api, httpapi.Op{
		Method: http.MethodGet, Path: "/api/v1/protected", OperationID: "getProtected",
		Tags: []string{"test"}, Summary: "Protected",
	}, httpapi.Policy{Permission: authz.PermissionAdminUsersRead}, func(context.Context, *struct{}) (*struct{}, error) {
		return &struct{}{}, nil
	})
}

func TestAuthenticateLimitsPerIPBeforeVerifying(t *testing.T) {
	const authMax = 3
	store := &countingKeyStore{}
	cfg := &config.Config{
		App:  config.AppConfig{Env: "production", BodyLimit: 1 << 20, RequestTimeout: 5 * time.Second},
		CORS: config.CORSConfig{AllowOrigins: []string{"*"}},
		RateLimit: config.RateLimitConfig{
			StrictMax: 5, StrictWindow: 60 * time.Second, NormalMax: 60, NormalWindow: 60 * time.Second, RelaxedMax: 120, RelaxedWindow: 60 * time.Second,
			// A long window keeps a boundary crossing out of this short test.
			AuthMax: authMax, AuthWindow: 3600 * time.Second,
		},
		Tenant: config.TenantConfig{DefaultSlug: "default"},
	}
	application := app.New(cfg, app.Deps{Sessions: allowAllSessions{}, APIKeyVerifier: apikey.NewVerifier(store, noPrincipals{})})
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

// noPrincipals knows no API-key owner; the unknown keys in this test never get
// that far.
type noPrincipals struct{}

func (noPrincipals) GetPrincipal(context.Context, uuid.UUID, uuid.UUID) (identitycontracts.Principal, error) {
	return identitycontracts.Principal{}, apperror.ErrNotFound
}
