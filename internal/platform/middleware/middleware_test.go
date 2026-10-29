package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func newTestApp() *fiber.App {
	return fiber.New(fiber.Config{
		ErrorHandler: apperrorhttp.FiberErrorHandler,
	})
}

// stubRoleSessions is a session store that reports one role for every token,
// so RequireRole tests exercise the snapshot path rather than a token claim.
type stubRoleSessions struct{ role string }

func (s stubRoleSessions) Validate(context.Context, session.Claims) (session.State, error) {
	return session.State{Role: s.role, Active: true, TenantActive: true}, nil
}

func (stubRoleSessions) Invalidate(context.Context, uuid.UUID) error { return nil }

func (stubRoleSessions) InvalidatePermissions(context.Context) error { return nil }

// ---------------------------------------------------------------------------
// JWTAuth tests
// ---------------------------------------------------------------------------

func TestJWTAuth_MissingHeader(t *testing.T) {
	app := newTestApp()
	app.Get("/protected", JWTAuth("secret", stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestJWTAuth_InvalidFormat(t *testing.T) {
	app := newTestApp()
	app.Get("/protected", JWTAuth("secret", stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "InvalidFormat")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestJWTAuth_InvalidToken(t *testing.T) {
	app := newTestApp()
	app.Get("/protected", JWTAuth("secret", stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer invalid.jwt.token")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestJWTAuth_ValidToken(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", JWTAuth(secret, stubRoleSessions{}), func(c fiber.Ctx) error {
		userID := requestctx.ActorID(c.Context())
		role := requestRoleForTest(c)

		if userID != testID(1) {
			t.Errorf("user_id = %s, want %s", userID, testID(1))
		}
		if role != "" {
			t.Errorf("role = %q, want empty without a session store", role)
		}

		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(testID(1), testID(1), secret, 24*time.Hour)

	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestJWTAuth_WrongSecret(t *testing.T) {
	app := newTestApp()
	app.Get("/protected", JWTAuth("correct-secret", stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(testID(1), testID(1), "wrong-secret", 24*time.Hour)

	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// tenantLocal simulates the global tenant middleware's resolution so JWT
// behavior can be tested without a resolver/database.
func tenantLocal(id uuid.UUID, slug string) fiber.Handler {
	return func(c fiber.Ctx) error {
		c.SetContext(tenant.WithID(c.Context(), id))
		fiber.Locals[string](c, "tenant_slug", slug)
		return c.Next()
	}
}

func TestJWTAuth_TenantFromClaimOverridesDefault(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", tenantLocal(testID(1), "default"), JWTAuth(secret, stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"tenant_id":   tenant.IDFromContext(c.Context()),
			"tenant_slug": tenant.SlugFromContext(c.Context()),
		})
	})

	accessToken, _ := token.GenerateWithTenant(testID(1), testID(2), secret, 24*time.Hour)
	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"tenant_id":"`+testID(2).String()+`"`) {
		t.Fatalf("body = %s, want tenant_id 2", body)
	}
	// The request resolved the default tenant before JWTAuth. Once the claim
	// overrides tenant_id, the old slug would be misleading, so it is cleared.
	if !strings.Contains(string(body), `"tenant_slug":""`) {
		t.Fatalf("body = %s, want tenant_slug cleared when the claim overrides it", body)
	}
}

func TestJWTAuth_NegativeTenantRejected(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", tenantLocal(testID(1), "default"), JWTAuth(secret, stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	claims := token.Claims{
		UserID:   testID(1),
		TenantID: uuid.Nil,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "fiber-golang-boilerplate",
			Audience:  jwt.ClaimStrings{"fiber-golang-boilerplate-api"},
		},
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	unsigned.Header["kid"] = token.KeyID(secret)
	accessToken, err := unsigned.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a negative tenant_id", resp.StatusCode)
	}
}

func TestJWTAuth_TenantHeaderMismatchRejected(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", tenantLocal(testID(3), "other"), JWTAuth(secret, stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(testID(1), testID(2), secret, 24*time.Hour)
	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set(tenant.Header, "other")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tenant mismatch") {
		t.Fatalf("body = %s, want tenant mismatch detail", body)
	}
}

func TestJWTAuth_TenantHeaderMatchAccepted(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", tenantLocal(testID(2), "acme"), JWTAuth(secret, stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(testID(1), testID(2), secret, 24*time.Hour)
	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set(tenant.Header, "acme")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestJWTAuth_LegacyTokenWithoutTenantRejected(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", tenantLocal(testID(3), "other"), JWTAuth(secret, stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"tenant_id": tenant.IDFromContext(c.Context())})
	})

	accessToken, _ := token.Generate(testID(1), secret, 24*time.Hour)
	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set(tenant.Header, "other")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a token without tenant_id", resp.StatusCode)
	}
}

func TestJWTAuth_RejectsTokenWithoutKeyID(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", JWTAuth(secret, stubRoleSessions{}), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	// Signed with the right secret but without the kid header that every
	// current token carries.
	unsigned := jwt.NewWithClaims(jwt.SigningMethodHS256, token.Claims{
		UserID:   testID(1),
		TenantID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "fiber-golang-boilerplate",
			Audience:  jwt.ClaimStrings{"fiber-golang-boilerplate-api"},
		},
	})
	accessToken, err := unsigned.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	req, _ := http.NewRequest("GET", "/protected", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a token without kid", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// RequestID tests
// ---------------------------------------------------------------------------

func TestRequestID_Generated(t *testing.T) {
	app := newTestApp()
	app.Get("/test", RequestID(), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}

	requestID := resp.Header.Get("X-Request-Id")
	if requestID == "" {
		t.Error("X-Request-Id should be generated when not provided")
	}
}

func TestRequestID_PassthroughFromTrustedProxy(t *testing.T) {
	// app.Test connections come from 0.0.0.0.
	app := fiber.New(fiber.Config{
		ErrorHandler:     apperrorhttp.FiberErrorHandler,
		TrustProxy:       true,
		TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"0.0.0.0"}},
	})
	app.Get("/test", RequestID(), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	req.Header.Set("X-Request-Id", "my-custom-id")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}

	requestID := resp.Header.Get("X-Request-Id")
	if requestID != "my-custom-id" {
		t.Errorf("X-Request-Id = %q, want my-custom-id", requestID)
	}
}

func TestRequestID_IgnoresUntrustedClientHeader(t *testing.T) {
	for name, cfg := range map[string]fiber.Config{
		"proxy trust disabled": {ErrorHandler: apperrorhttp.FiberErrorHandler},
		"peer not in trusted proxies": {
			ErrorHandler:     apperrorhttp.FiberErrorHandler,
			TrustProxy:       true,
			TrustProxyConfig: fiber.TrustProxyConfig{Proxies: []string{"10.0.0.1"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			app := fiber.New(cfg)
			app.Get("/test", RequestID(), func(c fiber.Ctx) error {
				return c.SendString(requestctx.RequestID(c.Context()))
			})

			req, _ := http.NewRequest("GET", "/test", http.NoBody)
			req.Header.Set("X-Request-Id", "forged-id")
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("app.Test failed: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			got := resp.Header.Get("X-Request-Id")
			if got == "forged-id" || got == "" || string(body) != got {
				t.Fatalf("X-Request-Id = %q (local %q), want a generated ID", got, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SecurityHeaders tests
// ---------------------------------------------------------------------------

func TestSecurityHeaders_Production(t *testing.T) {
	app := newTestApp()
	app.Get("/test", SecurityHeaders("production"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}

	expected := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"X-XSS-Protection":             "0",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Security-Policy":      "default-src 'none'; frame-ancestors 'none'",
		"Permissions-Policy":           "camera=(), microphone=(), geolocation=()",
		"Strict-Transport-Security":    "max-age=31536000; includeSubDomains",
	}

	for header, want := range expected {
		got := resp.Header.Get(header)
		if got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestSecurityHeaders_Local(t *testing.T) {
	app := newTestApp()
	app.Get("/test", SecurityHeaders("local"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}

	// Should NOT have HSTS in local
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS should not be set in local env, got %q", got)
	}

	// Other headers should still be present
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestSecurityHeaders_Test(t *testing.T) {
	app := newTestApp()
	app.Get("/test", SecurityHeaders("test"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}

	// Should NOT have HSTS in test env
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS should not be set in test env, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Recovery tests
// ---------------------------------------------------------------------------

func TestRecovery_PanicRecovered(t *testing.T) {
	app := newTestApp()
	app.Get("/panic", Recovery("test"), func(c fiber.Ctx) error {
		panic("test panic")
	})

	req, _ := http.NewRequest("GET", "/panic", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if result["code"] != "system.internal" {
		t.Errorf("code = %v, want internal_error", result["code"])
	}
	requestID := resp.Header.Get("X-Request-Id")
	if requestID == "" {
		t.Error("X-Request-Id should be set even for a panic before RequestID runs")
	}
	if got := result["requestId"]; got != requestID {
		t.Errorf("body requestId = %v, want header %q", got, requestID)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestRecovery_NormalPassthrough(t *testing.T) {
	app := newTestApp()
	app.Get("/ok", Recovery("test"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/ok", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRecovery_PanicAfterPartialWriteReplacesBody(t *testing.T) {
	app := newTestApp()
	app.Get("/partial", Recovery("test"), func(c fiber.Ctx) error {
		c.Status(fiber.StatusCreated)
		if err := c.SendString("partial success"); err != nil {
			return err
		}
		panic("after partial write")
	})

	req, _ := http.NewRequest("GET", "/partial", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if json.Valid(body) == false {
		t.Fatalf("body is not JSON: %q", body)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if result["code"] != "system.internal" {
		t.Fatalf("code = %v, want system.internal", result["code"])
	}
	if got := string(body); got != "" && got[0] != '{' {
		t.Fatalf("response body starts with %q, want a problem document", got[:1])
	}
}

// ---------------------------------------------------------------------------
// Timeout tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Logger tests
// ---------------------------------------------------------------------------

func TestLogger_Passthrough(t *testing.T) {
	app := newTestApp()
	app.Get("/test", Logger(nil), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestLogger_ErrorRoute(t *testing.T) {
	app := newTestApp()
	app.Get("/error", Logger(nil), func(c fiber.Ctx) error {
		return apperror.NewInternal("server error")
	})

	req, _ := http.NewRequest("GET", "/error", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestLogger_NotFoundRoute(t *testing.T) {
	app := newTestApp()
	app.Get("/missing", Logger(nil), func(c fiber.Ctx) error {
		return apperror.NewNotFound("not found")
	})

	req, _ := http.NewRequest("GET", "/missing", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Metrics tests
// ---------------------------------------------------------------------------

func TestMetrics_Passthrough(t *testing.T) {
	app := newTestApp()
	app.Get("/test", Metrics(nil), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Rate Limiter tests
// ---------------------------------------------------------------------------

func TestNewLimiter(t *testing.T) {
	app := newTestApp()
	app.Get("/test", NewLimiter(2, 60, "test", nil), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	// First 2 requests should succeed
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", "/test", http.NoBody)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("request %d: app.Test failed: %v", i, err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	// Third request should be rate limited
	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("rate limited request: status = %d, want 429", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Timeout tests
// ---------------------------------------------------------------------------

func TestTimeout_SetsContext(t *testing.T) {
	app := newTestApp()
	app.Get("/test", Timeout(5*time.Second, nil), func(c fiber.Ctx) error {
		deadline, ok := c.Context().Deadline()
		if !ok {
			t.Error("expected deadline to be set")
		}
		if deadline.Before(time.Now()) {
			t.Error("deadline should be in the future")
		}
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestTimeout_EnforcesDeadlineWhenHandlerIgnoresContext(t *testing.T) {
	app := newTestApp()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	app.Get("/slow", Timeout(50*time.Millisecond, nil), func(c fiber.Ctx) error {
		<-release
		return c.SendString("late")
	})

	req, _ := http.NewRequest("GET", "/slow", http.NoBody)
	start := time.Now()
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	defer resp.Body.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout response took %s, want a prompt deadline", elapsed)
	}
	if resp.StatusCode != fiber.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", resp.StatusCode)
	}
	if got := resp.Header.Get(fiber.HeaderContentType); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("content type = %q, want application/problem+json", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var problem apperror.ProblemDetails
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatalf("decode timeout problem: %v; body = %s", err, body)
	}
	if problem.Code != string(apperror.CodeSystemTimeout) || problem.Status != fiber.StatusGatewayTimeout {
		t.Errorf("problem = code %q status %d, want %q and 504", problem.Code, problem.Status, apperror.CodeSystemTimeout)
	}
}

func TestTimeout_HandlerObservingDeadlineGetsProblemResponse(t *testing.T) {
	app := newTestApp()
	app.Get("/slow", Timeout(50*time.Millisecond, nil), func(c fiber.Ctx) error {
		<-c.Context().Done()
		return c.Context().Err()
	})

	req, _ := http.NewRequest("GET", "/slow", http.NoBody)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusGatewayTimeout {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 504; body = %s", resp.StatusCode, body)
	}
}

func TestEffectiveStatus(t *testing.T) {
	if got := effectiveStatus(nil, nil, apperror.NewBadRequest("bad")); got != fiber.StatusBadRequest {
		t.Errorf("AppError status = %d, want 400", got)
	}
	if got := effectiveStatus(nil, nil, fiber.NewError(fiber.StatusTeapot, "teapot")); got != fiber.StatusTeapot {
		t.Errorf("fiber.Error status = %d, want 418", got)
	}
	if got := effectiveStatus(nil, nil, context.DeadlineExceeded); got != fiber.StatusGatewayTimeout {
		t.Errorf("deadline status = %d, want 504", got)
	}
	if got := effectiveStatus(nil, nil, fmt.Errorf("query: %w", context.Canceled)); got != apperror.StatusClientClosedRequest {
		t.Errorf("canceled status = %d, want 499", got)
	}
	if got := effectiveStatus(nil, nil, errors.New("generic")); got != fiber.StatusInternalServerError {
		t.Errorf("generic error status = %d, want 500", got)
	}
}

func TestRedactQuery(t *testing.T) {
	got := redactQuery("code=abc&state=xyz&access_token=secret&page=2")
	for _, leaked := range []string{"abc", "xyz", "secret"} {
		if strings.Contains(got, leaked) {
			t.Errorf("redactQuery() leaked %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, "page=2") {
		t.Errorf("redactQuery() dropped a safe parameter: %q", got)
	}
}

func TestValidRequestID(t *testing.T) {
	valid := []string{"abc-123", "req_1.2:3", "A"}
	for _, v := range valid {
		if !validRequestID(v) {
			t.Errorf("validRequestID(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "has space", "line\nbreak", strings.Repeat("a", 129), "<script>"}
	for _, v := range invalid {
		if validRequestID(v) {
			t.Errorf("validRequestID(%q) = true, want false", v)
		}
	}
}

func requestRoleForTest(c fiber.Ctx) string {
	subject, ok := authz.SubjectFromContext(c.Context())
	if !ok {
		return ""
	}
	return subject.Role
}
