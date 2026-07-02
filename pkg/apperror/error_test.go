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
		name       string
		fn         func(string) *AppError
		msg        string
		wantStatus int
		wantCode   string
	}{
		{"bad request", NewBadRequest, "bad", 400, "bad_request"},
		{"unauthorized", NewUnauthorized, "unauth", 401, "unauthorized"},
		{"forbidden", NewForbidden, "forbidden", 403, "forbidden"},
		{"not found", NewNotFound, "missing", 404, "not_found"},
		{"conflict", NewConflict, "dup", 409, "conflict"},
		{"internal", NewInternal, "oops", 500, "internal_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn(tt.msg)
			if err.Status != tt.wantStatus {
				t.Errorf("Status = %d, want %d", err.Status, tt.wantStatus)
			}
			if err.Code != tt.wantCode {
				t.Errorf("Code = %q, want %q", err.Code, tt.wantCode)
			}
			if err.Detail != tt.msg {
				t.Errorf("Detail = %q, want %q", err.Detail, tt.msg)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	fields := []FieldError{{Path: "email", Code: "invalid_email", Message: "email must be a valid email"}}
	err := NewValidation("validation failed", fields)

	if err.Status != 422 {
		t.Errorf("Status = %d, want 422", err.Status)
	}
	if err.Code != "validation_failed" {
		t.Errorf("Code = %q, want validation_failed", err.Code)
	}
	if len(err.Errors) != 1 || err.Errors[0].Path != "email" {
		t.Errorf("Errors not populated correctly: %+v", err.Errors)
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

// decodeProblem runs a handler and returns the parsed problem document + response.
func decodeProblem(t *testing.T, h fiber.Handler) (body map[string]any, status int, contentType string) {
	t.Helper()
	app := fiber.New(fiber.Config{ErrorHandler: FiberErrorHandler})
	app.Get("/x", h)
	req, _ := http.NewRequest("GET", "/x", http.NoBody)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test failed: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}
	return result, resp.StatusCode, resp.Header.Get("Content-Type")
}

func TestFiberErrorHandler_AppError(t *testing.T) {
	result, status, ctype := decodeProblem(t, func(c fiber.Ctx) error {
		return NewBadRequest("bad request test")
	})
	if status != 400 {
		t.Errorf("status = %d, want 400", status)
	}
	if ctype != "application/problem+json" {
		t.Errorf("content-type = %q, want application/problem+json", ctype)
	}
	if result["code"] != "bad_request" {
		t.Errorf("code = %v, want bad_request", result["code"])
	}
	if result["type"] != "/errors/bad-request" {
		t.Errorf("type = %v, want /errors/bad-request", result["type"])
	}
	if result["instance"] != "/x" {
		t.Errorf("instance = %v, want /x", result["instance"])
	}
	if result["timestamp"] == nil {
		t.Error("timestamp should be present")
	}
}

func TestFiberErrorHandler_Validation(t *testing.T) {
	result, status, _ := decodeProblem(t, func(c fiber.Ctx) error {
		return NewValidation("bad fields", []FieldError{{Path: "name", Code: "required", Message: "name is required"}})
	})
	if status != 422 {
		t.Errorf("status = %d, want 422", status)
	}
	errs, ok := result["errors"].([]any)
	if !ok || len(errs) != 1 {
		t.Fatalf("errors[] not present: %v", result["errors"])
	}
	first := errs[0].(map[string]any)
	if first["path"] != "name" || first["code"] != "required" {
		t.Errorf("field error = %v", first)
	}
}

func TestFiberErrorHandler_FiberError(t *testing.T) {
	result, status, _ := decodeProblem(t, func(c fiber.Ctx) error {
		return fiber.NewError(fiber.StatusNotFound, "route not found")
	})
	if status != 404 {
		t.Errorf("status = %d, want 404", status)
	}
	if result["code"] != "not_found" {
		t.Errorf("code = %v, want not_found", result["code"])
	}
}

func TestFiberErrorHandler_UnknownError(t *testing.T) {
	result, status, _ := decodeProblem(t, func(c fiber.Ctx) error {
		return errors.New("something unexpected")
	})
	if status != 500 {
		t.Errorf("status = %d, want 500", status)
	}
	if result["code"] != "internal_error" {
		t.Errorf("code = %v, want internal_error", result["code"])
	}
}
