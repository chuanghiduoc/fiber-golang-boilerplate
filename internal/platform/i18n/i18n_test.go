package i18n

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestNewLoadsEmbeddedCatalogs(t *testing.T) {
	bundle, err := New("")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := bundle.DefaultLocale(); got != "en" {
		t.Errorf("DefaultLocale() = %q, want en", got)
	}
	locales := bundle.SupportedLocales()
	if len(locales) != 2 || locales[0] != "en" || locales[1] != "vi" {
		t.Errorf("SupportedLocales() = %v, want [en vi]", locales)
	}
	for _, locale := range locales {
		if !bundle.Has(locale, "validation.required") {
			t.Errorf("catalog %q is missing validation.required", locale)
		}
	}
}

func TestNewRejectsUnsupportedDefault(t *testing.T) {
	if _, err := New("fr"); err == nil {
		t.Fatal("New(fr) should fail when no fr catalog is embedded")
	}
}

func TestNewTruncatesRegionalDefault(t *testing.T) {
	bundle, err := New("vi-VN")
	if err != nil {
		t.Fatalf("New(vi-VN) error = %v", err)
	}
	if got := bundle.DefaultLocale(); got != "vi" {
		t.Errorf("DefaultLocale() = %q, want vi", got)
	}
}

func TestNegotiate(t *testing.T) {
	bundle, err := New("en")
	if err != nil {
		t.Fatal(err)
	}
	vietnamese, err := New("vi")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		bundle *Bundle
		header string
		want   string
	}{
		{"empty falls back", bundle, "", "en"},
		{"exact", bundle, "vi", "vi"},
		{"case insensitive", bundle, "VI", "vi"},
		{"region truncation", bundle, "vi-VN", "vi"},
		{"underscore truncation", bundle, "vi_VN", "vi"},
		{"q-value ordering", bundle, "en;q=0.4, vi;q=0.9", "vi"},
		{"header order wins ties", bundle, "en;q=0.8, vi;q=0.8", "en"},
		{"unsupported then supported", bundle, "fr-FR, en;q=0.7", "en"},
		{"wildcard uses default", bundle, "fr, *;q=0.5", "en"},
		{"wildcard honors custom default", vietnamese, "fr, *;q=0.5", "vi"},
		{"zero q excluded", bundle, "vi;q=0, en;q=0.5", "en"},
		{"all zero falls back", bundle, "vi;q=0, en;q=0", "en"},
		{"invalid q falls back", bundle, "vi;q=bogus, en;q=0.5", "en"},
		{"q outside the RFC range is an x/text weight", bundle, "vi;q=2, en;q=0.5", "vi"},
		{"unknown only falls back", bundle, "de-DE", "en"},
		{"empty ranges ignored", bundle, ",, vi ,", "vi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.bundle.Negotiate(tt.header); got != tt.want {
				t.Errorf("Negotiate(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestTranslateInterpolates(t *testing.T) {
	bundle, err := New("en")
	if err != nil {
		t.Fatal(err)
	}

	en := bundle.Translate("en", "validation.too_short", map[string]any{"path": "name", "min": 3})
	if en != "The name field must be at least 3 characters." {
		t.Errorf("English translation = %q", en)
	}
	vi := bundle.Translate("vi-VN", "validation.too_long", map[string]any{"path": "code", "max": 5})
	if !strings.Contains(vi, "code") || !strings.Contains(vi, "5") {
		t.Errorf("Vietnamese translation missing params: %q", vi)
	}
	missingParam := bundle.Translate("en", "validation.required", map[string]any{})
	if missingParam != "The {path} field is required." {
		t.Errorf("missing params should preserve placeholders: %q", missingParam)
	}
	if got := bundle.Translate("en", "does.not.exist", nil); got != "does.not.exist" {
		t.Errorf("missing key = %q, want key", got)
	}
}

func TestMiddlewareSetsLocaleHeadersAndLocals(t *testing.T) {
	bundle, err := New("en")
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New()
	app.Use(Middleware(bundle))
	app.Get("/", func(c fiber.Ctx) error {
		if got := Locale(c); got != "vi" {
			return fiber.NewError(fiber.StatusInternalServerError, "locale local = "+got)
		}
		if got := LocaleFromContext(c.Context()); got != "vi" {
			return fiber.NewError(fiber.StatusInternalServerError, "context locale = "+got)
		}
		if got := bundle.Localize(c, "validation.required", map[string]any{"path": "email"}); got != "Trường email là bắt buộc." {
			return fiber.NewError(fiber.StatusInternalServerError, "localize = "+got)
		}
		return c.SendString("ok")
	})

	req, _ := http.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set(fiber.HeaderAcceptLanguage, "vi-VN, en;q=0.8")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(fiber.HeaderContentLanguage); got != "vi" {
		t.Errorf("Content-Language = %q, want vi", got)
	}
	if got := resp.Header.Get(fiber.HeaderVary); !strings.Contains(got, fiber.HeaderAcceptLanguage) {
		t.Errorf("Vary = %q, want Accept-Language", got)
	}
}

func TestMiddlewareNilBundlePassesThrough(t *testing.T) {
	app := fiber.New()
	app.Use(Middleware(nil))
	app.Get("/", func(c fiber.Ctx) error { return c.SendString("ok") })

	req, _ := http.NewRequest(http.MethodGet, "/", http.NoBody)
	req.Header.Set(fiber.HeaderAcceptLanguage, "vi")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get(fiber.HeaderContentLanguage); got != "" {
		t.Errorf("Content-Language = %q, want empty for nil bundle", got)
	}
}

func TestCatalogParityRejectsMissingKeys(t *testing.T) {
	reference := localeCatalog{"a": "A", "b": "B"}
	candidate := localeCatalog{"a": "A"}
	err := validateParity(map[string]localeCatalog{"en": reference, "vi": candidate}, []string{"en", "vi"})
	if err == nil || !strings.Contains(err.Error(), "missing keys: b") {
		t.Fatalf("validateParity() error = %v, want missing key b", err)
	}
}

func TestCatalogParityRejectsPlaceholderDrift(t *testing.T) {
	reference := localeCatalog{"validation.required": "The {path} field is required."}
	candidate := localeCatalog{"validation.required": "Trường field là bắt buộc."}
	err := validateParity(map[string]localeCatalog{"en": reference, "vi": candidate}, []string{"en", "vi"})
	if err == nil || !strings.Contains(err.Error(), "placeholders") {
		t.Fatalf("validateParity() error = %v, want placeholder mismatch", err)
	}
}

// TestCatalogsCoverErrorCatalog keeps the embedded titles in step with every
// stable apperror code. A new code must add en+vi entries in the same commit.
func TestCatalogsCoverErrorCatalog(t *testing.T) {
	bundle, err := New("en")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range apperror.NewCatalog().Codes() {
		key := "errors." + string(code) + ".title"
		for _, locale := range bundle.SupportedLocales() {
			if !bundle.Has(locale, key) {
				t.Errorf("catalog %q is missing %q", locale, key)
			}
		}
	}
}
