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
		{"exactly 64 runes", "abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUVWXYZ-0123456789", false}, // ggignore: test fixture, not a credential
		{"65 runes", "abcdefghijklmnopqrstuvwxyz-ABCDEFGHIJKLMNOPQRSTUVWXYZ-0123456789!", true},         // ggignore: test fixture, not a credential
		{"multibyte outside the old byte ceiling accepted", twoBytePassword(37), false},
		{"64 multibyte runes accepted", twoBytePassword(64), false},
		{"65 multibyte runes rejected", twoBytePassword(65), true},
		{"common password", "unbelievable", true},
		{"common password case-insensitive", "UNBELIEVABLE", true},
		{"trivially short", "Ab1!", true},
		{"single repeated character", strings.Repeat("x", 12), true},
		{"short pattern repeated", "abcabcabcabc", true},    // ggignore: test fixture, not a credential
		{"padded common password", "P@ssw0rd2026", true},    // ggignore: test fixture, not a credential
		{"leetspeak common password", "l3tm31n-2026", true}, // ggignore: test fixture, not a credential
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

// twoBytePassword builds distinct two-byte runes so a byte-length test does
// not accidentally become a repeated-pattern test.
func twoBytePassword(runes int) string {
	var b strings.Builder
	for r := rune(0x00E0); b.Len() < runes*2; r++ {
		b.WriteRune(r)
	}
	return b.String()
}

func TestValidatePassword_Codes(t *testing.T) {
	tests := map[string]string{
		"short":                 "weak_password",
		"unbelievable":          "password_common",
		"abcabcabcabc":          "password_repeated",
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

// NFKC folds compatibility characters before length and blocklist checks, so
// a full-width or ligature spelling cannot smuggle a short/common password
// past the policy.
func TestPasswordWeakness_NFKC(t *testing.T) {
	if got := NormalizePassword("\uFB01"); got != "fi" {
		t.Fatalf("NormalizePassword(ligature) = %q, want %q", got, "fi")
	}
	// Full-width digits become ASCII, exposing the common core "password".
	if got := PasswordWeakness("Password\uFF12\uFF10\uFF12\uFF16"); got != CodePasswordCommon {
		t.Errorf("full-width padded common password code = %q, want %q", got, CodePasswordCommon)
	}
	// Five ligatures are ten runes after NFKC but only five before it; the rune
	// length is measured after normalization.
	if got := PasswordWeakness("\uFB01\uFB01\uFB01\uFB01\uFB01"); got != CodeWeakPassword {
		t.Errorf("ligature password code = %q, want %q", got, CodeWeakPassword)
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
