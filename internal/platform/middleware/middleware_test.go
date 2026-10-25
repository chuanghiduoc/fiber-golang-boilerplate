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

// ---------------------------------------------------------------------------
// JWTAuth tests
// ---------------------------------------------------------------------------

func TestJWTAuth_MissingHeader(t *testing.T) {
	app := newTestApp()
	app.Get("/protected", JWTAuth("secret"), func(c fiber.Ctx) error {
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
	app.Get("/protected", JWTAuth("secret"), func(c fiber.Ctx) error {
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
	app.Get("/protected", JWTAuth("secret"), func(c fiber.Ctx) error {
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
	app.Get("/protected", JWTAuth(secret), func(c fiber.Ctx) error {
		userID := fiber.Locals[int64](c, "user_id")
		role := fiber.Locals[string](c, "role")
		email := fiber.Locals[string](c, "email")

		if userID != 1 {
			t.Errorf("user_id = %d, want 1", userID)
		}
		if role != "user" {
			t.Errorf("role = %q, want user", role)
		}
		if email != "test@example.com" {
			t.Errorf("email = %q, want test@example.com", email)
		}

		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", secret, 24)

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
	app.Get("/protected", JWTAuth("correct-secret"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(1, 1, "test@example.com", "user", "wrong-secret", 24)

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
func tenantLocal(id int64, slug string) fiber.Handler {
	return func(c fiber.Ctx) error {
		fiber.Locals[int64](c, "tenant_id", id)
		fiber.Locals[string](c, "tenant_slug", slug)
		return c.Next()
	}
}

func TestJWTAuth_TenantFromClaimOverridesDefault(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/protected", tenantLocal(1, "default"), JWTAuth(secret), func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"tenant_id":   fiber.Locals[int64](c, "tenant_id"),
			"tenant_slug": fiber.Locals[string](c, "tenant_slug"),
		})
	})

	accessToken, _ := token.GenerateWithTenant(1, 2, "user@example.com", "user", secret, 24)
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
	if !strings.Contains(string(body), `"tenant_id":2`) {
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
	app.Get("/protected", tenantLocal(1, "default"), JWTAuth(secret), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	claims := token.Claims{
		UserID:   1,
		TenantID: -1,
		Email:    "user@example.com",
		Role:     "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "fiber-golang-boilerplate",
			Audience:  jwt.ClaimStrings{"fiber-golang-boilerplate-api"},
		},
	}
	accessToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
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
	app.Get("/protected", tenantLocal(3, "other"), JWTAuth(secret), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(1, 2, "user@example.com", "user", secret, 24)
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
	app.Get("/protected", tenantLocal(2, "acme"), JWTAuth(secret), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(1, 2, "user@example.com", "user", secret, 24)
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
	app.Get("/protected", tenantLocal(3, "other"), JWTAuth(secret), func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"tenant_id": fiber.Locals[int64](c, "tenant_id")})
	})

	accessToken, _ := token.Generate(1, "user@example.com", "user", secret, 24)
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

// ---------------------------------------------------------------------------
// RequireRole tests
// ---------------------------------------------------------------------------

func TestRequireRole_Allowed(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/admin", JWTAuth(secret), RequireRole("admin"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(1, 1, "admin@example.com", "admin", secret, 24)

	req, _ := http.NewRequest("GET", "/admin", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRequireRole_Forbidden(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/admin", JWTAuth(secret), RequireRole("admin"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(1, 1, "user@example.com", "user", secret, 24)

	req, _ := http.NewRequest("GET", "/admin", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestRequireRole_MultipleRoles(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/shared", JWTAuth(secret), RequireRole("admin", "user"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.GenerateWithTenant(1, 1, "user@example.com", "user", secret, 24)

	req, _ := http.NewRequest("GET", "/shared", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
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
				return c.SendString(fiber.Locals[string](c, "request_id"))
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
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"X-XSS-Protection":          "1; mode=block",
		"Referrer-Policy":           "strict-origin-when-cross-origin",
		"Permissions-Policy":        "camera=(), microphone=(), geolocation=()",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
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
	if result["code"] != "internal_error" {
		t.Errorf("code = %v, want internal_error", result["code"])
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

// ---------------------------------------------------------------------------
// Timeout tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Logger tests
// ---------------------------------------------------------------------------

func TestLogger_Passthrough(t *testing.T) {
	app := newTestApp()
	app.Get("/test", Logger(), func(c fiber.Ctx) error {
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
	app.Get("/error", Logger(), func(c fiber.Ctx) error {
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
	app.Get("/missing", Logger(), func(c fiber.Ctx) error {
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
	app.Get("/test", Metrics(), func(c fiber.Ctx) error {
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
	app.Get("/test", Timeout(5*time.Second), func(c fiber.Ctx) error {
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

func TestEffectiveStatus(t *testing.T) {
	if got := effectiveStatus(nil, apperror.NewBadRequest("bad")); got != fiber.StatusBadRequest {
		t.Errorf("AppError status = %d, want 400", got)
	}
	if got := effectiveStatus(nil, fiber.NewError(fiber.StatusTeapot, "teapot")); got != fiber.StatusTeapot {
		t.Errorf("fiber.Error status = %d, want 418", got)
	}
	if got := effectiveStatus(nil, context.DeadlineExceeded); got != fiber.StatusGatewayTimeout {
		t.Errorf("deadline status = %d, want 504", got)
	}
	if got := effectiveStatus(nil, fmt.Errorf("query: %w", context.Canceled)); got != apperror.StatusClientClosedRequest {
		t.Errorf("canceled status = %d, want 499", got)
	}
	if got := effectiveStatus(nil, errors.New("generic")); got != fiber.StatusInternalServerError {
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
