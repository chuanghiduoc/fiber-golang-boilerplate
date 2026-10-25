package tenanthttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror/apperrorhttp"
)

type fakeResolver struct {
	tenants map[string]tenant.Tenant
	err     error
}

func (f fakeResolver) Resolve(_ context.Context, slug string) (tenant.Tenant, error) {
	if f.err != nil {
		return tenant.Tenant{}, f.err
	}
	t, ok := f.tenants[slug]
	if !ok {
		return tenant.Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

func newTestApp(r tenant.Resolver, defaultSlug string) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(Middleware(r, defaultSlug))
	app.Get("/", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"id": ID(c), "slug": Slug(c)})
	})
	return app
}

func doRequest(t *testing.T, app *fiber.App, header string) (resp *http.Response, result map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if header != "" {
		req.Header.Set(tenant.Header, header)
	}
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return resp, decoded
}

func TestMiddleware_DefaultTenant(t *testing.T) {
	r := fakeResolver{tenants: map[string]tenant.Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
	}}
	app := newTestApp(r, tenant.DefaultSlug)

	resp, body := doRequest(t, app, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(1) || body["slug"] != "default" {
		t.Fatalf("locals = %v, want tenant 1/default", body)
	}
}

func TestMiddleware_HeaderSelectsTenant(t *testing.T) {
	r := fakeResolver{tenants: map[string]tenant.Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
		"acme":    {ID: 2, Slug: "acme", Name: "Acme"},
	}}
	app := newTestApp(r, tenant.DefaultSlug)

	resp, body := doRequest(t, app, "acme")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(2) || body["slug"] != "acme" {
		t.Fatalf("locals = %v, want tenant 2/acme", body)
	}
}

func TestMiddleware_InvalidSlug(t *testing.T) {
	r := fakeResolver{tenants: map[string]tenant.Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
	}}
	app := newTestApp(r, tenant.DefaultSlug)

	for _, slug := range []string{"Acme", "has space", "under_score", "a/b", strings.Repeat("a", 64)} {
		resp, body := doRequest(t, app, slug)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("slug %q: status = %d, want 404", slug, resp.StatusCode)
		}
		if body["detail"] != "tenant not found" {
			t.Errorf("slug %q: detail = %v, want tenant not found", slug, body["detail"])
		}
	}
}

func TestMiddleware_UnknownSlug(t *testing.T) {
	r := fakeResolver{tenants: map[string]tenant.Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
	}}
	app := newTestApp(r, tenant.DefaultSlug)

	resp, body := doRequest(t, app, "missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if body["detail"] != "tenant not found" {
		t.Fatalf("detail = %v, want tenant not found", body["detail"])
	}
}

func TestMiddleware_RequireHeaderWhenDefaultEmpty(t *testing.T) {
	r := fakeResolver{tenants: map[string]tenant.Tenant{
		"acme": {ID: 2, Slug: "acme", Name: "Acme"},
	}}
	app := newTestApp(r, "")

	resp, body := doRequest(t, app, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if body["detail"] != "tenant not found" {
		t.Fatalf("detail = %v, want tenant not found", body["detail"])
	}

	resp, body = doRequest(t, app, "acme")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(2) || body["slug"] != "acme" {
		t.Fatalf("locals = %v, want tenant 2/acme", body)
	}
}

func TestMiddleware_NoResolverPassesThrough(t *testing.T) {
	app := newTestApp(nil, tenant.DefaultSlug)

	resp, body := doRequest(t, app, "anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(0) || body["slug"] != "" {
		t.Fatalf("locals = %v, want no tenant locals", body)
	}
}

func TestMiddleware_ResolverFailureIsInternalError(t *testing.T) {
	app := newTestApp(fakeResolver{err: context.DeadlineExceeded}, tenant.DefaultSlug)

	resp, body := doRequest(t, app, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if body["detail"] != "failed to resolve tenant" {
		t.Fatalf("detail = %v, want failed to resolve tenant", body["detail"])
	}
}

func newPathTestApp(r tenant.Resolver, defaultSlug, path string) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(Middleware(r, defaultSlug))
	app.Get(path, func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"id": ID(c), "slug": Slug(c)})
	})
	return app
}

func doPathRequest(t *testing.T, app *fiber.App, path, header string) (resp *http.Response, result map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if header != "" {
		req.Header.Set(tenant.Header, header)
	}
	resp, err = app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return resp, decoded
}

var tenantQueryTestResolver = fakeResolver{tenants: map[string]tenant.Tenant{
	"default": {ID: 1, Slug: "default", Name: "Default"},
	"acme":    {ID: 2, Slug: "acme", Name: "Acme"},
}}

func TestMiddleware_TenantQueryFallbackForBrowserAuth(t *testing.T) {
	for _, path := range []string{
		"/api/v1/auth/google",
		"/api/v1/auth/reset-password",
		"/api/v1/auth/verify-email",
	} {
		t.Run(path, func(t *testing.T) {
			app := newPathTestApp(tenantQueryTestResolver, "", path)
			resp, body := doPathRequest(t, app, path+"?tenant=acme", "")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			if body["id"] != float64(2) || body["slug"] != "acme" {
				t.Fatalf("locals = %v, want tenant 2/acme", body)
			}
		})
	}
}

func TestMiddleware_HeaderPrecedesTenantQuery(t *testing.T) {
	const path = "/api/v1/auth/reset-password"
	app := newPathTestApp(tenantQueryTestResolver, "", path)

	resp, body := doPathRequest(t, app, path+"?tenant=acme", "default")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(1) || body["slug"] != "default" {
		t.Fatalf("locals = %v, want header tenant 1/default", body)
	}
}

func TestMiddleware_TenantQueryIgnoredOutsideBrowserAuth(t *testing.T) {
	const path = "/api/v1/users/me"
	app := newPathTestApp(tenantQueryTestResolver, "", path)

	resp, body := doPathRequest(t, app, path+"?tenant=acme", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if body["detail"] != "tenant not found" {
		t.Fatalf("detail = %v, want tenant not found", body["detail"])
	}
}

type countingResolver struct {
	calls int
}

func (c *countingResolver) Resolve(_ context.Context, slug string) (tenant.Tenant, error) {
	c.calls++
	if slug == "acme" {
		return tenant.Tenant{ID: 2, Slug: "acme", Name: "Acme"}, nil
	}
	return tenant.Tenant{}, apperror.ErrNotFound
}

func TestMiddleware_OAuthCallbackSkipsResolution(t *testing.T) {
	resolver := &countingResolver{}
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(Middleware(resolver, ""))
	app.Get("/api/v1/auth/google/callback", func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{"id": ID(c), "slug": Slug(c)})
	})

	resp, err := app.Test(mustRequest(t, "/api/v1/auth/google/callback?state=x&code=y"))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (callback must bypass require-header)", resp.StatusCode)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 for the OAuth callback", resolver.calls)
	}
}

func TestMiddleware_WebhookInboundSkipsResolution(t *testing.T) {
	resolver := &countingResolver{}
	app := fiber.New(fiber.Config{ErrorHandler: apperrorhttp.FiberErrorHandler})
	app.Use(Middleware(resolver, ""))
	app.Post("/api/v1/webhooks/inbound/:id", func(c fiber.Ctx) error {
		return c.SendStatus(http.StatusAccepted)
	})

	req, err := http.NewRequest(http.MethodPost, "/api/v1/webhooks/inbound/42", http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (receiver must bypass require-header)", resp.StatusCode)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver calls = %d, want 0 for the public webhook receiver", resolver.calls)
	}
}

func mustRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}
