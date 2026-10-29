package apperror

import (
	"strings"
	"testing"
)

// requiredCodes pins the platform-wide catalog contract. Removing or renaming
// any of these breaks clients that branch on the code, so the test fails
// loudly. Module-owned codes are pinned by each module's catalog test.
var requiredCodes = []Code{
	CodePermissionDenied,
	CodeFeatureDisabled,
	CodeRateLimitExceeded,
	CodeIdempotencyKeyMismatch,
	CodeSystemInternal,
	CodeSystemTimeout,
	CodeSystemUnavailable,
	CodeSystemAuditUnavailable,
}

func TestCatalogSpecs(t *testing.T) {
	catalog := NewCatalog()
	for _, code := range catalog.Codes() {
		code := code
		t.Run(string(code), func(t *testing.T) {
			spec, ok := catalog.SpecFor(code)
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
	catalog := NewCatalog()
	for _, code := range requiredCodes {
		if _, ok := catalog.SpecFor(code); !ok {
			t.Errorf("required code %q is missing from the catalog", code)
		}
	}
}

func TestCatalogCodesUnique(t *testing.T) {
	catalog := NewCatalog()
	all := catalog.Codes()
	if len(all) != len(platformSpecs()) {
		t.Fatalf("Codes() returned %d codes, catalog has %d", len(all), len(platformSpecs()))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] >= all[i] {
			t.Errorf("Codes() not strictly sorted/unique at %q, %q", all[i-1], all[i])
		}
	}
}

func TestNewCodeIsAPureConstructor(t *testing.T) {
	err := NewCode(CodePermissionDenied, "public detail")
	if err == nil {
		t.Fatal("NewCode returned nil")
	}
	if err.Status != 0 {
		t.Errorf("Status = %d, want 0 (resolved only when rendered)", err.Status)
	}
	if err.Title != "" {
		t.Errorf("Title = %q, want empty (resolved only when rendered)", err.Title)
	}
	if err.Code != string(CodePermissionDenied) {
		t.Errorf("Code = %q, want %q", err.Code, CodePermissionDenied)
	}
	if err.Detail != "public detail" {
		t.Errorf("Detail = %q, want %q", err.Detail, "public detail")
	}
}

func TestCatalogProblemResolvesSpec(t *testing.T) {
	catalog := NewCatalog()
	for _, code := range catalog.Codes() {
		code := code
		t.Run(string(code), func(t *testing.T) {
			spec, ok := catalog.SpecFor(code)
			if !ok {
				t.Fatalf("SpecFor(%q) not found", code)
			}
			problem := catalog.Problem(NewCode(code, "public detail"), "req-1", "/api/v1/x")
			if problem.Status != spec.Status {
				t.Errorf("Status = %d, want %d", problem.Status, spec.Status)
			}
			if problem.Title != spec.Title {
				t.Errorf("Title = %q, want %q", problem.Title, spec.Title)
			}
			if problem.Code != string(code) {
				t.Errorf("Code = %q, want %q", problem.Code, code)
			}
			if problem.Detail != "public detail" {
				t.Errorf("Detail = %q, want %q", problem.Detail, "public detail")
			}
		})
	}
}

func TestCatalogDocsBaseURLIsInstanceScoped(t *testing.T) {
	first := NewCatalog()
	first.SetDocsBaseURL("https://docs.example.com/")
	second := NewCatalog()
	second.SetDocsBaseURL("  https://other.example.com///  ")

	firstProblem := first.Problem(NewBadRequest("bad"), "", "")
	if firstProblem.Type != "https://docs.example.com/errors/request-invalid" {
		t.Fatalf("first type = %q", firstProblem.Type)
	}
	secondProblem := second.Problem(NewBadRequest("bad"), "", "")
	if secondProblem.Type != "https://other.example.com/errors/request-invalid" {
		t.Fatalf("second type = %q", secondProblem.Type)
	}

	relative := NewCatalog().Problem(NewBadRequest("bad"), "", "")
	if relative.Type != "/errors/request-invalid" {
		t.Fatalf("relative type = %q", relative.Type)
	}
}

func TestCatalogProblemUnknownCodeFallsBack(t *testing.T) {
	catalog := NewCatalog()
	problem := catalog.Problem(NewCode("does.not.exist", "safe detail"), "req-1", "/api/v1/x")

	if problem.Status != 500 {
		t.Errorf("Status = %d, want 500", problem.Status)
	}
	if problem.Code != string(CodeSystemInternal) {
		t.Errorf("Code = %q, want %q", problem.Code, CodeSystemInternal)
	}
	spec, ok := catalog.SpecFor(CodeSystemInternal)
	if !ok {
		t.Fatal("system.internal missing from catalog")
	}
	if problem.Title != spec.Title {
		t.Errorf("Title = %q, want %q", problem.Title, spec.Title)
	}
	if problem.Detail != "safe detail" {
		t.Errorf("Detail = %q, want %q", problem.Detail, "safe detail")
	}
}

func TestCatalogProblemNormalizesFieldPathsWithoutMutatingInput(t *testing.T) {
	catalog := NewCatalog()
	original := []FieldError{
		{Path: "body.name", Code: "required"},
		{Path: "query.limit", Code: "invalid"},
		{Path: "body", Code: "invalid"},
		{Path: "name", Code: "required"},
	}
	problem := catalog.Problem(NewValidation("bad fields", original), "req-1", "/api/v1/x")

	want := []string{"name", "query.limit", "body", "name"}
	if len(problem.Errors) != len(want) {
		t.Fatalf("errors = %+v, want %d entries", problem.Errors, len(want))
	}
	for i, path := range want {
		if problem.Errors[i].Path != path {
			t.Errorf("errors[%d].path = %q, want %q", i, problem.Errors[i].Path, path)
		}
	}
	if original[0].Path != "body.name" {
		t.Errorf("rendering mutated the AppError: path = %q", original[0].Path)
	}
}

func TestCatalogAddModuleCodes(t *testing.T) {
	catalog := NewCatalog()
	spec := Spec{Code: "widget.locked", Status: 409, Title: "Widget locked", DocPath: "/errors/widget-locked"}
	if err := catalog.Add(Provider{Owner: "widgets", Specs: []Spec{spec}}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	problem := catalog.Problem(NewCode("widget.locked", "busy"), "", "")
	if problem.Status != 409 || problem.Code != "widget.locked" || problem.Title != "Widget locked" {
		t.Fatalf("Problem = %+v, want registered spec", problem)
	}
	if owner, _ := catalog.OwnerOf("widget.locked"); owner != "widgets" {
		t.Fatalf("OwnerOf = %q, want widgets", owner)
	}
	if err := catalog.Add(Provider{Owner: "widgets", Specs: []Spec{spec}}); err != nil {
		t.Fatalf("identical re-add must be a no-op: %v", err)
	}
}

func TestCatalogAddRejectsConflicts(t *testing.T) {
	valid := Spec{Code: "x.ok", Status: 400, Title: "Bad", DocPath: "/errors/x"}
	changedPermissionDenied := Spec{Code: CodePermissionDenied, Status: 400, Title: "Permission denied", DocPath: "/errors/permission-denied"}
	cases := map[string]struct {
		owner string
		specs []Spec
		want  string
	}{
		"platform code": {
			"intruder",
			[]Spec{{Code: CodePermissionDenied, Status: 403, Title: "Permission denied", DocPath: "/errors/permission-denied"}},
			"already owned",
		},
		"changed spec":  {platformOwner, []Spec{changedPermissionDenied}, "different spec"},
		"bad code":      {"x", []Spec{{Code: "NoDots", Status: 400, Title: "t", DocPath: "/errors/x"}}, "invalid code"},
		"bad status":    {"x", []Spec{{Code: "x.bad_status", Status: 200, Title: "t", DocPath: "/errors/x"}}, "status"},
		"empty title":   {"x", []Spec{{Code: "x.empty_title", Status: 400, DocPath: "/errors/x"}}, "title"},
		"bad doc path":  {"x", []Spec{{Code: "x.bad_doc", Status: 400, Title: "t", DocPath: "x"}}, "DocPath"},
		"missing owner": {"", []Spec{valid}, "owner is required"},
		"duplicate":     {"x", []Spec{valid, valid}, "listed twice"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := NewCatalog().Add(Provider{Owner: tc.owner, Specs: tc.specs})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCatalogsAreIndependentAndSealable(t *testing.T) {
	first := NewCatalog()
	second := NewCatalog()
	spec := Spec{Code: "widget.locked", Status: 409, Title: "Widget locked", DocPath: "/errors/widget-locked"}
	if err := first.Add(Provider{Owner: "widgets", Specs: []Spec{spec}}); err != nil {
		t.Fatalf("Add to first: %v", err)
	}
	if _, ok := second.SpecFor("widget.locked"); ok {
		t.Fatal("adding to one catalog must not affect another")
	}
	if err := first.Add(Provider{Owner: "widgets", Specs: []Spec{{Code: "widget.missing", Status: 404, Title: "Missing", DocPath: "/errors/widget-missing"}}}); err != nil {
		t.Fatalf("Add to first: %v", err)
	}
	if _, ok := second.SpecFor("widget.missing"); ok {
		t.Fatal("adding to one catalog must not affect another")
	}

	first.Seal()
	if err := first.Add(Provider{Owner: "widgets", Specs: []Spec{{Code: "widget.other", Status: 400, Title: "Other", DocPath: "/errors/widget-other"}}}); err == nil {
		t.Fatal("Add after Seal must fail")
	}

	if _, ok := PlatformCatalog().SpecFor("widget.locked"); ok {
		t.Fatal("platform catalog must not contain module codes")
	}
}
