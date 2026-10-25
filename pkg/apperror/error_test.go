package apperror

import (
	"context"
	"errors"
	"fmt"
	"testing"
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

func TestAppErrorIsNotFound(t *testing.T) {
	if !errors.Is(NewNotFound("user not found"), ErrNotFound) {
		t.Fatal("a 404 AppError must match ErrNotFound")
	}
	if !errors.Is(fmt.Errorf("wrapped: %w", NewNotFound("x")), ErrNotFound) {
		t.Fatal("a wrapped 404 AppError must match ErrNotFound")
	}
	if errors.Is(NewUnauthorized("x"), ErrNotFound) {
		t.Fatal("a non-404 AppError must not match ErrNotFound")
	}
	if errors.Is(NewNotFound("x"), context.Canceled) {
		t.Fatal("a 404 AppError must not match unrelated sentinels")
	}
}
