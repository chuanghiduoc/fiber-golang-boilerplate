package apperror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestConstructors(t *testing.T) {
	tests := []struct {
		name      string
		fn        func(string) *AppError
		msg       string
		wantCode  int
		wantECode string
	}{
		{"bad request", NewBadRequest, "bad", 400, "BAD_REQUEST"},
		{"unauthorized", NewUnauthorized, "unauth", 401, "UNAUTHORIZED"},
		{"forbidden", NewForbidden, "forbidden", 403, "FORBIDDEN"},
		{"not found", NewNotFound, "missing", 404, "NOT_FOUND"},
		{"internal", NewInternal, "oops", 500, "INTERNAL_ERROR"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn(tt.msg)
			if err.Code != tt.wantCode {
				t.Errorf("Code = %d, want %d", err.Code, tt.wantCode)
			}
			if err.ErrorCode != tt.wantECode {
				t.Errorf("ErrorCode = %q, want %q", err.ErrorCode, tt.wantECode)
			}
			if err.Message != tt.msg {
				t.Errorf("Message = %q, want %q", err.Message, tt.msg)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	details := map[string]string{"field": "required"}
	err := NewValidation("validation failed", details)

	if err.Code != 422 {
		t.Errorf("Code = %d, want 422", err.Code)
	}
	if err.ErrorCode != "VALIDATION_ERROR" {
		t.Errorf("ErrorCode = %q, want VALIDATION_ERROR", err.ErrorCode)
	}
	if err.Details == nil {
		t.Error("Details should not be nil")
	}
}

func TestAppError_Error(t *testing.T) {
	err := NewBadRequest("test message")
	if err.Error() != "test message" {
		t.Errorf("Error() = %q, want %q", err.Error(), "test message")
	}
}

func TestErrNotFound_Sentinel(t *testing.T) {
	wrapped := fmt.Errorf("wrap: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("errors.Is should match ErrNotFound through wrapping")
	}
}

func TestFiberErrorHandler_AppError(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: FiberErrorHandler,
	})
	app.Get("/app-error", func(c fiber.Ctx) error {
		return NewBadRequest("bad request test")
	})

	req, _ := http.NewRequest("GET", "/app-error", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	if result["success"] != false {
		t.Error("success should be false")
	}
}

func TestFiberErrorHandler_AppErrorWithDetails(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: FiberErrorHandler,
	})
	app.Get("/validation", func(c fiber.Ctx) error {
		return NewValidation("validation failed", map[string]string{"name": "required"})
	})

	req, _ := http.NewRequest("GET", "/validation", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 422 {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	errObj, ok := result["error"].(map[string]any)
	if !ok {
		t.Fatal("error field not found")
	}
	if errObj["details"] == nil {
		t.Error("details should not be nil")
	}
}

func TestFiberErrorHandler_FiberError(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: FiberErrorHandler,
	})
	app.Get("/fiber-error", func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusNotFound, "route not found")
	})

	req, _ := http.NewRequest("GET", "/fiber-error", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	errObj := result["error"].(map[string]any)
	if errObj["code"] != "FIBER_ERROR" {
		t.Errorf("error code = %v, want FIBER_ERROR", errObj["code"])
	}
}

func TestFiberErrorHandler_UnknownError(t *testing.T) {
	app := fiber.New(fiber.Config{
		ErrorHandler: FiberErrorHandler,
	})
	app.Get("/unknown", func(c fiber.Ctx) error {
		return errors.New("something unexpected")
	})

	req, _ := http.NewRequest("GET", "/unknown", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}
