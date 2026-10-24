package middleware

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

func TestNewLimiter_KeysAPIKeysSeparately(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		switch c.Get("X-Test-Key") {
		case "one":
			fiber.Locals[int64](c, apikey.LocalAPIKeyID, 1)
		case "two":
			fiber.Locals[int64](c, apikey.LocalAPIKeyID, 2)
		}
		return c.Next()
	})
	app.Get("/", NewLimiter(1, 60, "normal", NewLimiterStorage(cache.NewMemoryCache())), func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	do := func(key string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "/", http.NoBody)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Test-Key", key)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if status := do("one"); status != fiber.StatusOK {
		t.Fatalf("first request status = %d, want 200", status)
	}
	if status := do("one"); status != fiber.StatusTooManyRequests {
		t.Fatalf("second request for key one status = %d, want 429", status)
	}
	if status := do("two"); status != fiber.StatusOK {
		t.Fatalf("request for key two status = %d, want an independent 200 bucket", status)
	}
}
