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
		{"bad request", NewBadRequest, "bad", 400, "request.invalid"},
		{"auth.unauthenticated", NewUnauthorized, "unauth", 401, "auth.unauthenticated"},
		{"permission.denied", NewForbidden, "permission.denied", 403, "permission.denied"},
		{"not found", NewNotFound, "missing", 404, "resource.not_found"},
		{"resource.conflict", NewConflict, "dup", 409, "resource.conflict"},
		{"internal", NewInternal, "oops", 500, "system.internal"},
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

func TestNewForStatusUsesNamespacedCodes(t *testing.T) {
	cases := map[int]string{
		400: "request.invalid",
		401: "auth.unauthenticated",
		403: "permission.denied",
		404: "resource.not_found",
		409: "resource.conflict",
		412: "resource.precondition_failed",
		413: "request.payload_too_large",
		415: "request.unsupported_media_type",
		422: "request.validation_failed",
		429: "rate_limit.exceeded",
		500: "system.internal",
		503: "system.unavailable",
		504: "system.timeout",
	}
	for status, want := range cases {
		if got := NewForStatus(status, "detail").Code; got != want {
			t.Errorf("NewForStatus(%d).Code = %q, want %q", status, got, want)
		}
	}
}

func TestNewValidation(t *testing.T) {
	fields := []FieldError{{Path: "email", Code: "invalid_email", Message: "email must be a valid email"}}
	err := NewValidation("validation failed", fields)

	if err.Status != 422 {
		t.Errorf("Status = %d, want 422", err.Status)
	}
	if err.Code != "request.validation_failed" {
		t.Errorf("Code = %q, want validation_failed", err.Code)
	}
	if len(err.Errors) != 1 || err.Errors[0].Path != "email" {
		t.Errorf("Errors not populated correctly: %+v", err.Errors)
	}
}

func TestAppErrorProblemNormalizesFieldPaths(t *testing.T) {
	err := NewValidation("bad fields", []FieldError{
		{Path: "body.name", Code: "required"},
		{Path: "query.limit", Code: "invalid"},
	})
	problem := err.Problem("req-1", "/api/v1/x")

	if got := problem.Errors[0].Path; got != "name" {
		t.Errorf("body field path = %q, want name", got)
	}
	if got := problem.Errors[1].Path; got != "query.limit" {
		t.Errorf("query field path = %q, want query.limit", got)
	}
	if err.Errors[0].Path != "body.name" {
		t.Errorf("Problem mutated the source error: %q", err.Errors[0].Path)
	}
}

func TestAppError_Error(t *testing.T) {
	err := NewBadRequest("test message")
	if err.Error() != "test message" {
		t.Errorf("Error() = %q, want %q", err.Error(), "test message")
	}
}

func TestInternalKeepsCauseOutOfPublicDetail(t *testing.T) {
	cause := errors.New("database password leaked in driver error")
	err := Internal(cause, "Internal Server Error")
	if !errors.Is(err, cause) {
		t.Fatal("Internal must unwrap to its cause")
	}
	if err.Detail != "Internal Server Error" {
		t.Fatalf("Detail = %q, want the public text only", err.Detail)
	}
	if err.Status != 500 || err.Code != string(CodeSystemInternal) {
		t.Fatalf("status/code = %d/%q", err.Status, err.Code)
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
