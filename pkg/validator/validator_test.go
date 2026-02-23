package validator

import (
	"errors"
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
		{"valid", "Abcdef1!", false},
		{"too short", "Ab1!", true},
		{"no uppercase", "abcdef1!", true},
		{"no lowercase", "ABCDEF1!", true},
		{"no digit", "Abcdefg!", true},
		{"no special", "Abcdefg1", true},
		{"exactly 8 chars", "Abcdef1@", false},
		{"73 bytes rejected", "Aa1!" + repeat('x', 69), true},
		{"72 bytes accepted", "Aa1!" + repeat('x', 68), false},
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

func TestValidatePassword_BcryptLimit72(t *testing.T) {
	// 72 bytes should pass
	pw72 := "Aa1!" + repeat('x', 68) // 4 + 68 = 72
	if len(pw72) != 72 {
		t.Fatalf("expected 72, got %d", len(pw72))
	}
	if err := ValidateStruct(passwordReq{Password: pw72}); err != nil {
		t.Errorf("72-byte password should be valid: %v", err)
	}

	// 73 bytes should fail
	pw73 := "Aa1!" + repeat('x', 69) // 4 + 69 = 73
	if len(pw73) != 73 {
		t.Fatalf("expected 73, got %d", len(pw73))
	}
	if err := ValidateStruct(passwordReq{Password: pw73}); err == nil {
		t.Error("73-byte password should be rejected")
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
	if appErr.Code != 422 {
		t.Errorf("expected status 422, got %d", appErr.Code)
	}
}

func repeat(ch byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ch
	}
	return string(b)
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
	if appErr.Details == nil {
		t.Fatal("expected validation details")
	}
	details, ok := appErr.Details.(map[string]string)
	if !ok {
		t.Fatalf("expected map[string]string details, got %T", appErr.Details)
	}
	if _, ok := details["Email"]; !ok {
		t.Error("expected Email field in details")
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

func TestValidateStruct_Valid(t *testing.T) {
	err := ValidateStruct(emailReq{Email: "valid@example.com"})
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}
