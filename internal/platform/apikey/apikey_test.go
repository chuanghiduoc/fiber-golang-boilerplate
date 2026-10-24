package apikey

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeStore struct {
	key     Key
	findErr error
	touched []time.Time
}

func (s *fakeStore) FindByHash(_ context.Context, hash string) (Key, error) {
	if s.findErr != nil {
		return Key{}, s.findErr
	}
	if hash != s.key.Hash {
		return Key{}, apperror.ErrNotFound
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

func newTestKey(t *testing.T) (Credentials, Key) {
	t.Helper()
	credentials, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return credentials, Key{
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

func TestGenerate_FormatAndHash(t *testing.T) {
	credentials, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(credentials.Key, DefaultPrefix+"_") {
		t.Fatalf("key = %q, want prefix %q", credentials.Key, DefaultPrefix+"_")
	}
	if prefix, err := ParsePrefix(credentials.Key); err != nil || prefix != DefaultPrefix {
		t.Fatalf("parse = %q, %v; want %q", prefix, err, DefaultPrefix)
	}
	if !Valid(credentials.Key) {
		t.Fatal("generated key must be valid")
	}
	if len(credentials.Hash) != KeyHashBytes*2 {
		t.Fatalf("hash length = %d, want %d hex characters", len(credentials.Hash), KeyHashBytes*2)
	}
	if credentials.Hash != Hash(credentials.Key) {
		t.Fatal("stored hash must be SHA-256 of the complete key")
	}
	other, err := Generate()
	if err != nil {
		t.Fatalf("generate second: %v", err)
	}
	if other.Key == credentials.Key {
		t.Fatal("two generated keys must differ")
	}
}

func TestGenerate_RejectsInvalidPrefix(t *testing.T) {
	for _, prefix := range []string{"", "A", "UPPER", "has space", "toolongprefixname"} {
		if _, err := GenerateWithPrefix(prefix); !errors.Is(err, ErrMalformedKey) {
			t.Errorf("GenerateWithPrefix(%q) error = %v, want ErrMalformedKey", prefix, err)
		}
	}
}

func TestParsePrefix_RejectsMalformedKeys(t *testing.T) {
	valid, _ := Generate()
	invalid := []string{
		"",
		"fgb",
		"fgb_",
		"FGB_" + strings.Repeat("a", 43),
		"fgb_short",
		"fgb_" + strings.Repeat("!", 43),
		"fgb_" + strings.Repeat("a", 42),
	}
	for _, key := range invalid {
		if Valid(key) {
			t.Errorf("key %q must be invalid", key)
		}
	}
	if !Valid(valid.Key) {
		t.Fatal("valid key rejected")
	}
}

func fixedClock(ts time.Time) Option {
	return WithClock(func() time.Time { return ts })
}

func TestVerifier_SuccessAndLastUsedTouch(t *testing.T) {
	credentials, key := newTestKey(t)
	store := &fakeStore{key: key}
	now := time.Unix(1_700_000_100, 0).UTC()
	principal := identitycontracts.Principal{ID: 42, TenantID: 1, Email: "owner@test.com", Role: authz.RoleUser, TenantStatus: identitycontracts.TenantStatusActive}
	verifier := NewVerifier(store, fakePrincipals{principal: principal}, fixedClock(now))

	verified, err := verifier.Verify(context.Background(), credentials.Key, 1)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.Key.ID != key.ID || verified.Principal.ID != 42 {
		t.Fatalf("verified = %+v, want key 7/principal 42", verified)
	}
	if len(verified.Key.Scopes) != 1 || verified.Key.Scopes[0] != testScopeRead {
		t.Fatalf("scopes = %v, want apikeytest:read", verified.Key.Scopes)
	}
	if len(store.touched) != 1 || !store.touched[0].Equal(now) {
		t.Fatalf("touched = %v, want one write at %v", store.touched, now)
	}

	// No second write inside the touch interval.
	verifier.Verify(context.Background(), credentials.Key, 1)
	if len(store.touched) != 1 {
		t.Fatalf("touches = %d, want 1 within interval", len(store.touched))
	}

	// A write is attempted again once the interval has elapsed.
	verifier = NewVerifier(store, fakePrincipals{principal: principal}, fixedClock(now.Add(2*time.Minute)))
	verifier.Verify(context.Background(), credentials.Key, 1)
	if len(store.touched) != 2 {
		t.Fatalf("touches = %d, want 2 after interval", len(store.touched))
	}
}

func TestVerifier_RejectsInvalidStates(t *testing.T) {
	credentials, baseKey := newTestKey(t)
	now := time.Unix(1_700_000_100, 0).UTC()
	principal := identitycontracts.Principal{ID: 42, TenantID: 1, Role: authz.RoleUser, TenantStatus: identitycontracts.TenantStatusActive}

	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	pastGrace := now.Add(-time.Minute)
	futureGrace := now.Add(time.Hour)

	tests := []struct {
		name       string
		mutate     func(*Key)
		tenant     int64
		wantStatus int
	}{
		{
			name:       "expired",
			mutate:     func(k *Key) { k.ExpiresAt = &past },
			tenant:     1,
			wantStatus: 401,
		},
		{
			name:       "revoked",
			mutate:     func(k *Key) { k.RevokedAt = &past },
			tenant:     1,
			wantStatus: 401,
		},
		{
			name: "rotation grace active",
			mutate: func(k *Key) {
				k.RotatedAt = &past
				k.RotationGraceUntil = &future
			},
			tenant:     1,
			wantStatus: 200,
		},
		{
			name: "rotation grace expired",
			mutate: func(k *Key) {
				k.RotatedAt = &past
				k.RotationGraceUntil = &pastGrace
			},
			tenant:     1,
			wantStatus: 401,
		},
		{
			name: "rotated without grace",
			mutate: func(k *Key) {
				k.RotatedAt = &past
			},
			tenant:     1,
			wantStatus: 401,
		},
		{
			name:       "tenant mismatch",
			mutate:     func(*Key) {},
			tenant:     2,
			wantStatus: 403,
		},
		{
			name: "expiry extends beyond active rotation grace",
			mutate: func(k *Key) {
				k.RotatedAt = &past
				k.ExpiresAt = &future
				k.RotationGraceUntil = &futureGrace
			},
			tenant:     1,
			wantStatus: 200,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := baseKey
			tt.mutate(&key)
			store := &fakeStore{key: key}
			verifier := NewVerifier(store, fakePrincipals{principal: principal}, fixedClock(now))
			_, err := verifier.Verify(context.Background(), credentials.Key, tt.tenant)
			if tt.wantStatus == 200 {
				if err != nil {
					t.Fatalf("verify: %v", err)
				}
				return
			}
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || appErr.Status != tt.wantStatus {
				t.Fatalf("error = %v, want status %d", err, tt.wantStatus)
			}
		})
	}
}

func TestVerifier_RejectsUnknownMalformedAndInactiveOwner(t *testing.T) {
	credentials, key := newTestKey(t)
	now := time.Unix(1_700_000_100, 0).UTC()

	tests := []struct {
		name       string
		raw        string
		store      *fakeStore
		principals identitycontracts.PrincipalReader
	}{
		{
			name:  "unknown hash",
			raw:   credentials.Key,
			store: &fakeStore{key: key, findErr: apperror.ErrNotFound},
		},
		{
			name:  "malformed",
			raw:   "fgb_short",
			store: &fakeStore{key: key},
		},
		{
			name:       "owner missing",
			raw:        credentials.Key,
			store:      &fakeStore{key: key},
			principals: fakePrincipals{err: apperror.ErrNotFound},
		},
		{
			name:  "owner tenant mismatch",
			raw:   credentials.Key,
			store: &fakeStore{key: key},
			principals: fakePrincipals{principal: identitycontracts.Principal{
				ID: 42, TenantID: 2, Role: authz.RoleUser, TenantStatus: identitycontracts.TenantStatusActive,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := NewVerifier(tt.store, tt.principals, fixedClock(now))
			_, err := verifier.Verify(context.Background(), tt.raw, 0)
			var appErr *apperror.AppError
			if !errors.As(err, &appErr) || appErr.Status != 401 {
				t.Fatalf("error = %v, want 401", err)
			}
		})
	}
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
	verifier := NewVerifier(store, fakePrincipals{principal: identitycontracts.Principal{
		ID: 42, TenantID: 1, Email: "owner@test.com", Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive,
	}}, fixedClock(now))

	jwtCalled := false
	jwt := func(c fiber.Ctx) error {
		jwtCalled = true
		fiber.Locals[string](c, LocalAuthMethod, AuthMethodJWT)
		return c.SendString("jwt")
	}

	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
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
			"apiKeyID":   fiber.Locals[int64](c, LocalAPIKeyID),
			"authMethod": fiber.Locals[string](c, LocalAuthMethod),
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
	req.Header.Set(fiber.HeaderAuthorization, APIKeyScheme+" "+credentials.Key)
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
	if payload["apiKeyID"].(float64) != 7 || payload["authMethod"] != AuthMethodAPIKey {
		t.Fatalf("payload = %+v, want key 7 auth_method api_key", payload)
	}

	// An explicit conflicting tenant header is 403.
	req, _ = http.NewRequest(http.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, APIKeyScheme+" "+credentials.Key)
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
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Get("/me", Authenticate(nil, nil), func(c fiber.Ctx) error { return c.SendString("ok") })

	req, _ := http.NewRequest(http.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, APIKeyScheme+" fgb_"+strings.Repeat("a", 43))
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestVerifier_RejectsSuspendedTenant(t *testing.T) {
	credentials, key := newTestKey(t)
	store := &fakeStore{key: key}
	verifier := NewVerifier(store, fakePrincipals{principal: identitycontracts.Principal{
		ID: 42, TenantID: 1, Role: authz.RoleUser, TenantStatus: "suspended",
	}})

	_, err := verifier.Verify(context.Background(), credentials.Key, 0)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) || appErr.Status != fiber.StatusUnauthorized {
		t.Fatalf("err = %v, want 401 for a suspended tenant", err)
	}
	if len(store.touched) != 0 {
		t.Fatal("a rejected key must not update last_used_at")
	}
}

func TestAuthenticate_InstallsActorContextAndClearsOverriddenSlug(t *testing.T) {
	credentials, key := newTestKey(t)
	key.TenantID = 3
	store := &fakeStore{key: key}
	verifier := NewVerifier(store, fakePrincipals{principal: identitycontracts.Principal{
		ID: 42, TenantID: 3, Role: authz.RoleAdmin, TenantStatus: identitycontracts.TenantStatusActive,
	}})

	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
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
			"localSlug":  tenant.Slug(c),
			"ctxSlug":    tenant.SlugFromContext(ctx),
			"ctxTenant":  tenant.IDFromContext(ctx),
		})
	})

	req, _ := http.NewRequest(http.MethodGet, "/me", http.NoBody)
	req.Header.Set(fiber.HeaderAuthorization, APIKeyScheme+" "+credentials.Key)
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
