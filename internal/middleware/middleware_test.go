package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

func newTestApp() *fiber.App {
	return fiber.New(fiber.Config{
		ErrorHandler: apperror.FiberErrorHandler,
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

	accessToken, _ := token.Generate(1, "test@example.com", "user", secret, 24)

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

	accessToken, _ := token.Generate(1, "test@example.com", "user", "wrong-secret", 24)

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

// ---------------------------------------------------------------------------
// RequireRole tests
// ---------------------------------------------------------------------------

func TestRequireRole_Allowed(t *testing.T) {
	secret := "test-secret"
	app := newTestApp()
	app.Get("/admin", JWTAuth(secret), RequireRole("admin"), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	accessToken, _ := token.Generate(1, "admin@example.com", "admin", secret, 24)

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

	accessToken, _ := token.Generate(1, "user@example.com", "user", secret, 24)

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

	accessToken, _ := token.Generate(1, "user@example.com", "user", secret, 24)

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

	requestID := resp.Header.Get("X-Request-ID")
	if requestID == "" {
		t.Error("X-Request-ID should be generated when not provided")
	}
}

func TestRequestID_Passthrough(t *testing.T) {
	app := newTestApp()
	app.Get("/test", RequestID(), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	req, _ := http.NewRequest("GET", "/test", http.NoBody)
	req.Header.Set("X-Request-ID", "my-custom-id")

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}

	requestID := resp.Header.Get("X-Request-ID")
	if requestID != "my-custom-id" {
		t.Errorf("X-Request-ID = %q, want my-custom-id", requestID)
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
		"Referrer-Policy":            "strict-origin-when-cross-origin",
		"Permissions-Policy":         "camera=(), microphone=(), geolocation=()",
		"Strict-Transport-Security":  "max-age=31536000; includeSubDomains",
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
	if result["success"] != false {
		t.Error("success should be false")
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
	app.Get("/test", NewLimiter(2, 60), func(c fiber.Ctx) error {
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
