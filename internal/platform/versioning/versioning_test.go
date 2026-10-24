package versioning

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func TestParseAcceptVersion(t *testing.T) {
	tests := []struct {
		value string
		want  Version
		ok    bool
	}{
		{"1", V1, true},
		{"v1", V1, true},
		{" V1 ", V1, true},
		{"2", V2, true},
		{"v2", V2, true},
		{"V2", V2, true},
		{"", "", false},
		{"3", "", false},
		{"v3", "", false},
		{"v1.0", "", false},
		{"latest", "", false},
	}
	for _, tt := range tests {
		got, ok := ParseAcceptVersion(tt.value)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseAcceptVersion(%q) = (%q, %v), want (%q, %v)", tt.value, got, ok, tt.want, tt.ok)
		}
	}
}

func doRequest(t *testing.T, app *fiber.App, path string, headers map[string]string) (resp *http.Response, body []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err = app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func decodeProblem(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode problem: %v (body %q)", err, raw)
	}
	return body
}

func TestMiddlewareEchoesVersionAndVary(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(Middleware(V1, nil))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("ok") })

	for _, header := range []string{"", "1", "v1", "V1"} {
		t.Run(header, func(t *testing.T) {
			resp, raw := doRequest(t, app, "/x", map[string]string{Header: header})
			if resp.StatusCode != fiber.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", resp.StatusCode, raw)
			}
			if got := resp.Header.Get(ResponseHeader); got != "v1" {
				t.Errorf("API-Version = %q, want v1", got)
			}
			if got := resp.Header.Get(fiber.HeaderVary); !strings.Contains(got, Header) {
				t.Errorf("Vary = %q, want %s", got, Header)
			}
		})
	}
}

func TestMiddlewareRejectsMismatchedAcceptVersion(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(Middleware(V1, nil))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("ok") })

	for _, header := range []string{"2", "v2", "3", "latest"} {
		t.Run(header, func(t *testing.T) {
			resp, raw := doRequest(t, app, "/x", map[string]string{Header: header})
			if resp.StatusCode != fiber.StatusNotAcceptable {
				t.Fatalf("status = %d, want 406", resp.StatusCode)
			}
			body := decodeProblem(t, raw)
			if body["code"] != string(apperror.CodeAPIVersionUnsupported) {
				t.Errorf("code = %v, want %s", body["code"], apperror.CodeAPIVersionUnsupported)
			}
			if body["type"] != "/errors/api-version-unsupported" {
				t.Errorf("type = %v", body["type"])
			}
			if got := resp.Header.Get(ResponseHeader); got != "v1" {
				t.Errorf("API-Version on error = %q, want v1", got)
			}
		})
	}
}

func TestMiddlewareV2Fixture(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(Middleware(V2, nil))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("v2") })

	resp, _ := doRequest(t, app, "/x", map[string]string{Header: "2"})
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get(ResponseHeader); got != "v2" {
		t.Errorf("API-Version = %q, want v2", got)
	}
}

func TestMiddlewareDeprecationHeaders(t *testing.T) {
	deprecatedAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	sunsetAt := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	cfg := &Config{
		Default: V1,
		Policies: map[Version]Policy{
			V1: {
				DeprecatedAt: deprecatedAt,
				SunsetAt:     sunsetAt,
				Successor:    V2,
				DocsURL:      "https://docs.example.com/api/v1",
			},
		},
	}
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use(Middleware(V1, cfg))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("ok") })

	resp, _ := doRequest(t, app, "/x", nil)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Deprecation"), "@"+strconv.FormatInt(deprecatedAt.Unix(), 10); got != want {
		t.Errorf("Deprecation = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("Sunset"), sunsetAt.Format(http.TimeFormat); got != want {
		t.Errorf("Sunset = %q, want %q", got, want)
	}
	link := resp.Header.Get("Link")
	if !strings.Contains(link, "<https://docs.example.com/api/v1>; rel=\"deprecation\"") {
		t.Errorf("Link missing deprecation docs: %q", link)
	}
	if !strings.Contains(link, "</api/v2>; rel=\"successor-version\"") {
		t.Errorf("Link missing successor: %q", link)
	}
}

func TestMiddlewareFutureDeprecationHasNoHeaders(t *testing.T) {
	cfg := &Config{Policies: map[Version]Policy{
		V1: {DeprecatedAt: time.Now().Add(24 * time.Hour), SunsetAt: time.Now().Add(48 * time.Hour), Successor: V2},
	}}
	app := fiber.New()
	app.Use(Middleware(V1, cfg))
	app.Get("/x", func(c fiber.Ctx) error { return c.SendString("ok") })

	resp, _ := doRequest(t, app, "/x", nil)
	if got := resp.Header.Get("Deprecation"); got != "" {
		t.Errorf("Deprecation = %q, want empty before DeprecatedAt", got)
	}
	if got := resp.Header.Get("Sunset"); got != "" {
		t.Errorf("Sunset = %q, want empty before DeprecatedAt", got)
	}
}

func TestGuardUnsupportedVersion(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use("/api", Guard(V1, V2))
	app.Get("/api/v1/x", func(c fiber.Ctx) error { return c.SendString("v1") })
	app.Get("/api/v2/x", func(c fiber.Ctx) error { return c.SendString("v2") })

	for _, path := range []string{"/api/v1/x", "/api/v2/x"} {
		resp, _ := doRequest(t, app, path, nil)
		if resp.StatusCode != fiber.StatusOK {
			t.Errorf("%s status = %d, want 200", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/api/v3/x", "/api/v10/x"} {
		resp, raw := doRequest(t, app, path, nil)
		if resp.StatusCode != fiber.StatusNotAcceptable {
			t.Errorf("%s status = %d, want 406", path, resp.StatusCode)
		}
		if body := decodeProblem(t, raw); body["code"] != string(apperror.CodeAPIVersionUnsupported) {
			t.Errorf("%s code = %v", path, body["code"])
		}
		if got := resp.Header.Get(fiber.HeaderVary); !strings.Contains(got, Header) {
			t.Errorf("%s Vary = %q, want %s", path, got, Header)
		}
	}
	// Non-version paths keep ordinary routing behavior.
	resp, _ := doRequest(t, app, "/api/status", nil)
	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("/api/status status = %d, want 404", resp.StatusCode)
	}
}

func TestGuardOnlySupportsMountedVersions(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
	app.Use("/api", Guard(V1))
	app.Get("/api/v1/x", func(c fiber.Ctx) error { return c.SendString("v1") })

	resp, raw := doRequest(t, app, "/api/v2/x", nil)
	if resp.StatusCode != fiber.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", resp.StatusCode)
	}
	body := decodeProblem(t, raw)
	if body["code"] != string(apperror.CodeAPIVersionUnsupported) {
		t.Errorf("code = %v", body["code"])
	}
}

func TestNilConfigDefaults(t *testing.T) {
	var cfg *Config
	if got := cfg.DefaultVersion(); got != V1 {
		t.Errorf("DefaultVersion() = %q, want v1", got)
	}
	if got := cfg.PolicyFor(V1); got != (Policy{}) {
		t.Errorf("PolicyFor(v1) = %+v, want zero", got)
	}
	if got := (&Config{}).DefaultVersion(); got != V1 {
		t.Errorf("zero DefaultVersion() = %q, want v1", got)
	}
	if got := (&Config{Default: V2}).DefaultVersion(); got != V2 {
		t.Errorf("explicit DefaultVersion() = %q, want v2", got)
	}
}
