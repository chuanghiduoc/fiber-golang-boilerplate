package apperror

import (
	"strings"
	"testing"
)

// requiredCodes pins the public catalog contract. Removing or renaming any
// of these breaks clients that branch on the code, so the test fails loudly.
var requiredCodes = []Code{
	CodeAuthInvalidCredentials,
	CodeAuthEmailNotVerified,
	CodeAuthRefreshInvalid,
	CodeUserEmailTaken,
	CodeUserNotFound,
	CodeUserLastAdmin,
	CodeFileNotFound,
	CodeFileTooLarge,
	CodeFileUnsupportedType,
	CodeFileNotDeleted,
	CodePermissionDenied,
	CodeRateLimitExceeded,
	CodeSystemInternal,
	CodeSystemTimeout,
	CodeSystemUnavailable,
}

func TestCatalogSpecs(t *testing.T) {
	for _, code := range AllCodes() {
		code := code
		t.Run(string(code), func(t *testing.T) {
			spec, ok := SpecFor(code)
			if !ok {
				t.Fatalf("SpecFor(%q) not found", code)
			}
			if spec.Status < 400 || spec.Status > 599 {
				t.Errorf("Status = %d, want 400..599", spec.Status)
			}
			if strings.TrimSpace(spec.Title) == "" {
				t.Error("Title must not be empty")
			}
			if strings.TrimSpace(spec.DocPath) == "" {
				t.Error("DocPath must not be empty")
			}
			if !strings.HasPrefix(spec.DocPath, "/errors/") {
				t.Errorf("DocPath = %q, want /errors/ prefix", spec.DocPath)
			}
			if strings.Contains(string(code), " ") {
				t.Errorf("Code = %q must not contain spaces", code)
			}
		})
	}
}

func TestCatalogRequiredCodesPresent(t *testing.T) {
	for _, code := range requiredCodes {
		if _, ok := SpecFor(code); !ok {
			t.Errorf("required code %q is missing from the catalog", code)
		}
	}
}

func TestCatalogCodesUnique(t *testing.T) {
	seen := make(map[Code]bool, len(requiredCodes))
	for _, code := range requiredCodes {
		if seen[code] {
			t.Errorf("duplicate code %q", code)
		}
		seen[code] = true
	}

	all := AllCodes()
	if len(all) != len(catalog) {
		t.Fatalf("AllCodes() returned %d codes, catalog has %d", len(all), len(catalog))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] >= all[i] {
			t.Errorf("AllCodes() not strictly sorted/unique at %q, %q", all[i-1], all[i])
		}
	}
}

func TestNewCodeUsesSpec(t *testing.T) {
	for _, code := range AllCodes() {
		code := code
		t.Run(string(code), func(t *testing.T) {
			spec, ok := SpecFor(code)
			if !ok {
				t.Fatalf("SpecFor(%q) not found", code)
			}
			err := NewCode(code, "public detail")
			if err == nil {
				t.Fatal("NewCode returned nil")
			}
			if err.Status != spec.Status {
				t.Errorf("Status = %d, want %d", err.Status, spec.Status)
			}
			if err.Title != spec.Title {
				t.Errorf("Title = %q, want %q", err.Title, spec.Title)
			}
			if err.Code != string(code) {
				t.Errorf("Code = %q, want %q", err.Code, code)
			}
			if err.Detail != "public detail" {
				t.Errorf("Detail = %q, want %q", err.Detail, "public detail")
			}
			if err.Errors != nil {
				t.Errorf("Errors = %v, want nil", err.Errors)
			}
		})
	}
}

func TestNewCodeUnknownFallback(t *testing.T) {
	err := NewCode(Code("does.not.exist"), "safe detail")

	if err.Status != 500 {
		t.Errorf("Status = %d, want 500", err.Status)
	}
	if err.Code != string(CodeSystemInternal) {
		t.Errorf("Code = %q, want %q", err.Code, CodeSystemInternal)
	}

	spec, ok := SpecFor(CodeSystemInternal)
	if !ok {
		t.Fatal("system.internal missing from catalog")
	}
	if err.Title != spec.Title {
		t.Errorf("Title = %q, want %q", err.Title, spec.Title)
	}
	if err.Detail != "safe detail" {
		t.Errorf("Detail = %q, want %q", err.Detail, "safe detail")
	}
}
