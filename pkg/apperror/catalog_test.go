package apperror

import (
	"strings"
	"testing"
)

// requiredCodes pins the platform-wide catalog contract. Removing or renaming
// any of these breaks clients that branch on the code, so the test fails
// loudly. Module-owned codes are pinned by each module's catalog test.
var requiredCodes = []Code{
	CodeUserLastAdmin,
	CodePermissionDenied,
	CodeFeatureDisabled,
	CodeRateLimitExceeded,
	CodeAPIVersionUnsupported,
	CodeIdempotencyKeyMismatch,
	CodeSystemInternal,
	CodeSystemTimeout,
	CodeSystemUnavailable,
	CodeSystemAuditUnavailable,
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
	if len(all) != len(state.Load().specs) {
		t.Fatalf("AllCodes() returned %d codes, catalog has %d", len(all), len(state.Load().specs))
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

func TestRegisterModuleCodes(t *testing.T) {
	spec := Spec{Status: 409, Title: "Widget locked", DocPath: "/errors/widget-locked"}
	if err := Register("widgets", map[Code]Spec{"widget.locked": spec}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := NewCode("widget.locked", "busy"); got.Status != 409 || got.Code != "widget.locked" {
		t.Fatalf("NewCode = %+v, want registered spec", got)
	}
	if owner, _ := OwnerOf("widget.locked"); owner != "widgets" {
		t.Fatalf("OwnerOf = %q, want widgets", owner)
	}
	if err := Register("widgets", map[Code]Spec{"widget.locked": spec}); err != nil {
		t.Fatalf("identical re-registration must be a no-op: %v", err)
	}
}

func TestRegisterRejectsConflicts(t *testing.T) {
	valid := Spec{Status: 400, Title: "Bad", DocPath: "/errors/x"}
	cases := map[string]struct {
		owner string
		specs map[Code]Spec
		want  string
	}{
		"platform code": {"intruder", map[Code]Spec{CodePermissionDenied: platformSpecs[CodePermissionDenied]}, "already owned"},
		"changed spec":  {platformOwner, map[Code]Spec{CodePermissionDenied: valid}, "different spec"},
		"bad code":      {"x", map[Code]Spec{"NoDots": valid}, "invalid code"},
		"bad status":    {"x", map[Code]Spec{"x.bad_status": {Status: 200, Title: "t", DocPath: "/errors/x"}}, "status"},
		"empty title":   {"x", map[Code]Spec{"x.empty_title": {Status: 400, DocPath: "/errors/x"}}, "title"},
		"bad doc path":  {"x", map[Code]Spec{"x.bad_doc": {Status: 400, Title: "t", DocPath: "x"}}, "DocPath"},
		"missing owner": {"", map[Code]Spec{"x.no_owner": valid}, "owner is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Register(tc.owner, tc.specs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Register error = %v, want containing %q", err, tc.want)
			}
		})
	}
	if _, ok := SpecFor("x.no_owner"); ok {
		t.Fatal("a rejected registration must not be applied")
	}
}
