package validator

import (
	"errors"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type passwordReq struct {
	Password string `validate:"required,password"`
}

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name    string
		pw      string
		wantErr bool
	}{
		{"valid long phrase", "correct horse battery staple", false},
		{"valid without composition", "alllowercase", false},
		{"exactly 12 runes", "abcdefghijkl", false},
		{"11 runes", "abcdefghijk", true},
		{"exactly 64 runes", strings.Repeat("a", 64), false},
		{"65 runes", strings.Repeat("a", 65), true},
		{"72 bytes accepted", strings.Repeat("é", 36), false},
		{"73 bytes rejected", strings.Repeat("é", 37), true},
		{"common password", "unbelievable", true},
		{"common password case-insensitive", "UNBELIEVABLE", true},
		{"trivially short", "Ab1!", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateStruct(passwordReq{Password: tt.pw})
			if tt.wantErr && err == nil {
				t.Errorf("expected error for password %q (len=%d)", tt.pw, len(tt.pw))
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error for password %q (len=%d): %v", tt.pw, len(tt.pw), err)
			}
		})
	}
}

func TestValidatePassword_Codes(t *testing.T) {
	tests := map[string]string{
		"short":                 "weak_password",
		"unbelievable":          "password_common",
		strings.Repeat("a", 65): "weak_password",
	}
	for pw, want := range tests {
		err := ValidateStruct(passwordReq{Password: pw})
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || len(appErr.Errors) == 0 {
			t.Fatalf("password %q: expected a validation error, got %v", pw, err)
		}
		if got := appErr.Errors[0].Code; got != want {
			t.Errorf("password %q code = %q, want %q", pw, got, want)
		}
	}
}

func TestPasswordContainsEmailLocalPart(t *testing.T) {
	tests := []struct {
		name     string
		password string
		email    string
		want     bool
	}{
		{"equals local part", "alice", "alice@example.com", true},
		{"contains local part", "alice-in-wonderland", "Alice@Example.com", true},
		{"unrelated password", "correct horse battery", "alice@example.com", false},
		{"empty email", "correct horse battery", "", false},
		{"two-letter local part is ignored", "my-password", "ab@example.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PasswordContainsEmailLocalPart(tt.password, tt.email); got != tt.want {
				t.Errorf("PasswordContainsEmailLocalPart(%q, %q) = %v, want %v", tt.password, tt.email, got, tt.want)
			}
		})
	}
}

func TestValidateStruct_Required(t *testing.T) {
	err := ValidateStruct(passwordReq{Password: ""})
	if err == nil {
		t.Fatal("expected error for empty password")
	}
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if appErr.Status != 422 {
		t.Errorf("expected status 422, got %d", appErr.Status)
	}
}

// ---------------------------------------------------------------------------
// formatError branch coverage
// ---------------------------------------------------------------------------

type emailReq struct {
	Email string `validate:"required,email"`
}

func TestValidateStruct_Email(t *testing.T) {
	err := ValidateStruct(emailReq{Email: "not-email"})
	if err == nil {
		t.Fatal("expected error for invalid email")
	}
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if len(appErr.Errors) == 0 {
		t.Fatal("expected validation field errors")
	}
	if appErr.Errors[0].Code != "invalid_email" {
		t.Errorf("expected code invalid_email, got %q", appErr.Errors[0].Code)
	}
}

type minReq struct {
	Name string `validate:"min=3"`
}

func TestValidateStruct_Min(t *testing.T) {
	err := ValidateStruct(minReq{Name: "ab"})
	if err == nil {
		t.Fatal("expected error for min length violation")
	}
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if got := appErr.Errors[0].Params["min"]; got != "3" {
		t.Errorf("min param = %q, want 3", got)
	}
}

type maxReq struct {
	Code string `validate:"max=5"`
}

func TestValidateStruct_Max(t *testing.T) {
	err := ValidateStruct(maxReq{Code: "toolongstring"})
	if err == nil {
		t.Fatal("expected error for max length violation")
	}
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	if got := appErr.Errors[0].Params["max"]; got != "5" {
		t.Errorf("max param = %q, want 5", got)
	}
}

type urlReq struct {
	URL string `validate:"url"`
}

func TestValidateStruct_DefaultTag(t *testing.T) {
	err := ValidateStruct(urlReq{URL: "not-a-url"})
	if err == nil {
		t.Fatal("expected error for invalid url")
	}
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
}

type nestedItem struct {
	Name string `json:"name" validate:"required"`
}

type nestedReq struct {
	Title string       `json:"title" validate:"required"`
	Items []nestedItem `json:"items" validate:"dive"`
	Owner struct {
		Email string `json:"email" validate:"email"`
	} `json:"owner"`
}

func TestValidateStruct_NestedPathsUseJSONNames(t *testing.T) {
	req := nestedReq{Items: []nestedItem{{Name: "ok"}, {Name: ""}}}
	req.Owner.Email = "not-email"
	err := ValidateStruct(req)
	var appErr *apperror.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected AppError, got %T", err)
	}
	paths := make(map[string]string, len(appErr.Errors))
	for _, field := range appErr.Errors {
		paths[field.Path] = field.Message
	}
	for path, message := range map[string]string{
		"title":         "title is required",
		"items[1].name": "items[1].name is required",
		"owner.email":   "owner.email must be a valid email",
	} {
		if got, ok := paths[path]; !ok || got != message {
			t.Errorf("path %q message = %q (present %v), want %q; all: %v", path, got, ok, message, paths)
		}
	}
	if len(paths) != 3 {
		t.Errorf("errors = %v, want exactly 3 paths", paths)
	}
}

func TestValidateStruct_Valid(t *testing.T) {
	err := ValidateStruct(emailReq{Email: "valid@example.com"})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}
