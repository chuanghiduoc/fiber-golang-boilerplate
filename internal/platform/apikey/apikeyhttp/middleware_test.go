package apikeyhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

type fakeStore struct {
	key     apikey.Key
	findErr error
	touched []time.Time
}

func (s *fakeStore) FindByHash(_ context.Context, hash string) (apikey.Key, error) {
	if s.findErr != nil {
		return apikey.Key{}, s.findErr
	}
	if hash != s.key.Hash {
		return apikey.Key{}, apperror.ErrNotFound
	}
	return s.key, nil
}

func (s *fakeStore) TouchLastUsed(_ context.Context, id int64, at time.Time) error {
	if id != s.key.ID {
		return apperror.ErrNotFound
	}
	s.touched = append(s.touched, at)
	lastUsed := at
	s.key.LastUsedAt = &lastUsed
	return nil
}

type fakePrincipals struct {
	principal identitycontracts.Principal
	err       error
}

func (p fakePrincipals) GetPrincipal(context.Context, int64, int64) (identitycontracts.Principal, error) {
	return p.principal, p.err
}

func newTestKey(t *testing.T) (apikey.Credentials, apikey.Key) {
	t.Helper()
	credentials, err := apikey.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return credentials, apikey.Key{
		ID:        7,
		TenantID:  1,
		UserID:    42,
		Name:      "ci",
		Prefix:    credentials.Prefix,
		Hash:      credentials.Hash,
		Scopes:    []authz.Permission{testScopeRead},
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
}

func fixedClock(ts time.Time) apikey.Option {
	return apikey.WithClock(func() time.Time { return ts })
}

func TestEffectiveScopes_CurrentRoleBoundsKey(t *testing.T) {
	scopes := []authz.Permission{
		authz.PermissionAdminUsersRead,
		testScopeRead,
	}
	got := effectiveScopes(authz.RoleUser, scopes)
	if len(got) != 1 || got[0] != testScopeRead {
		t.Fatalf("effective scopes = %v, want only apikeytest:read after demotion", got)
	}

	got = effectiveScopes(authz.RoleAdmin, scopes)
	if len(got) != 2 {
		t.Fatalf("effective scopes = %v, want both scopes for admin", got)
	}

	if got := effectiveScopes("", scopes); len(got) != 2 {
		t.Fatalf("effective scopes without a role = %v, want scopes unchanged", got)
	}
}

func TestAuthenticate_DelegatesJWTsAndSetsAPIKeyLocals(t *testing.T) {
	credentials, key := newTestKey(t)
	now := time.Unix(1_700_000_100, 0).UTC()
	store := &fakeStore{key: key}
	verifier := apikey.NewVerifier(store, fakePrincipals{principal: identitycontracts.Principal{
		ID: 42, TenantID: 1, Email: "owner@test.com", Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive,
	}}, fixedClock(now))

	jwtCalled := false
	jwt := func(c fiber.Ctx) error {
		jwtCalled = true
		fiber.Locals[string](c, apikey.LocalAuthMethod, apikey.AuthMethodJWT)
		return c.SendString("jwt")
	}

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		if slug := c.Get(tenant.Header); slug == "other" {
			fiber.Locals[int64](c, "tenant_id", 2)
			fiber.Locals[string](c, "tenant_slug", "other")
		}
		return c.Next()
	})
	app.Get("/me", Authenticate(jwt, verifier), func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"userID":     fiber.Locals[int64](c, "user_id"),
			"tenantID":   fiber.Locals[int64](c, "tenant_id"),
			"apiKeyID":   fiber.Locals[int64](c, apikey.LocalAPIKeyID),
			"authMethod": fiber.Locals[string](c, apikey.LocalAuthMethod),
		})
	})

	// Bearer and missing headers delegate to the JWT handler.
	for _, header := range []string{"Bearer token", ""} {
		req, _ := http.NewRequest(http.MethodGet, "/me", http.NoBody)
		if header != "" {
			req.Header.Set(fiber.HeaderAuthorization, header)
		}
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	}
	if !jwtCalled {
		t.Fatal("JWT handler was not delegated to")
	}

	// An API key sets the API-key locals and an explicit-scope subject.
	req, _ := http.NewRequest(http.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, apikey.APIKeyScheme+" "+credentials.Key)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, body)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["userID"].(float64) != 42 || payload["tenantID"].(float64) != 1 {
		t.Fatalf("payload = %+v, want user 42 tenant 1", payload)
	}
	if payload["apiKeyID"].(float64) != 7 || payload["authMethod"] != apikey.AuthMethodAPIKey {
		t.Fatalf("payload = %+v, want key 7 auth_method api_key", payload)
	}

	// An explicit conflicting tenant header is 403.
	req, _ = http.NewRequest(http.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, apikey.APIKeyScheme+" "+credentials.Key)
	req.Header.Set(tenant.Header, "other")
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403 tenant mismatch", resp.StatusCode)
	}
}

func TestAuthenticate_NilVerifierRejectsApiKey(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Get("/me", Authenticate(nil, nil), func(c fiber.Ctx) error { return c.SendString("ok") })

	req, _ := http.NewRequest(http.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, apikey.APIKeyScheme+" fgb_"+strings.Repeat("a", 43))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthenticate_InstallsActorContextAndClearsOverriddenSlug(t *testing.T) {
	credentials, key := newTestKey(t)
	key.TenantID = 3
	store := &fakeStore{key: key}
	verifier := apikey.NewVerifier(store, fakePrincipals{principal: identitycontracts.Principal{
		ID: 42, TenantID: 3, Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive,
	}})

	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		// Simulate the tenant middleware resolving a header-less request to
		// the default tenant.
		fiber.Locals[string](c, "request_id", "req-1")
		fiber.Locals[int64](c, "tenant_id", 1)
		fiber.Locals[string](c, "tenant_slug", "default")
		c.SetContext(tenant.WithIdentity(c.Context(), 1, "default"))
		return c.Next()
	})
	app.Get("/me", Authenticate(nil, verifier), func(c fiber.Ctx) error {
		ctx := c.Context()
		info := audit.RequestInfoFromContext(ctx)
		if info == nil {
			return c.Status(fiber.StatusInternalServerError).SendString("no audit info")
		}
		return c.JSON(fiber.Map{
			"actor":      requestctx.ActorID(ctx),
			"auditActor": info.ActorID,
			"auditRole":  info.ActorRole,
			"requestID":  info.RequestID,
			"localSlug":  tenanthttp.Slug(c),
			"ctxSlug":    tenant.SlugFromContext(ctx),
			"ctxTenant":  tenant.IDFromContext(ctx),
		})
	})

	req, _ := http.NewRequest(http.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, apikey.APIKeyScheme+" "+credentials.Key)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d (body=%s)", resp.StatusCode, body)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["actor"].(float64) != 42 || payload["auditActor"].(float64) != 42 ||
		payload["auditRole"] != authz.RoleAdmin || payload["requestID"] != "req-1" {
		t.Fatalf("actor context = %+v, want actor 42/admin with request id", payload)
	}
	if payload["localSlug"] != "" || payload["ctxSlug"] != "" || payload["ctxTenant"].(float64) != 3 {
		t.Fatalf("tenant = %+v, want key tenant 3 with cleared slug", payload)
	}
}
