package tenant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeResolver struct {
	tenants map[string]Tenant
	err     error
}

func (f fakeResolver) Resolve(_ context.Context, slug string) (Tenant, error) {
	if f.err != nil {
		return Tenant{}, f.err
	}
	t, ok := f.tenants[slug]
	if !ok {
		return Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

func newTestApp(r Resolver, defaultSlug string) *fiber.App {
	app := fiber.New(fiber.Config{ErrorHandler: apperror.FiberErrorHandler})
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
		req.Header.Set(Header, header)
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
	r := fakeResolver{tenants: map[string]Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
	}}
	app := newTestApp(r, DefaultSlug)

	resp, body := doRequest(t, app, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(1) || body["slug"] != "default" {
		t.Fatalf("locals = %v, want tenant 1/default", body)
	}
}

func TestMiddleware_HeaderSelectsTenant(t *testing.T) {
	r := fakeResolver{tenants: map[string]Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
		"acme":    {ID: 2, Slug: "acme", Name: "Acme"},
	}}
	app := newTestApp(r, DefaultSlug)

	resp, body := doRequest(t, app, "acme")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(2) || body["slug"] != "acme" {
		t.Fatalf("locals = %v, want tenant 2/acme", body)
	}
}

func TestMiddleware_InvalidSlug(t *testing.T) {
	r := fakeResolver{tenants: map[string]Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
	}}
	app := newTestApp(r, DefaultSlug)

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
	r := fakeResolver{tenants: map[string]Tenant{
		"default": {ID: 1, Slug: "default", Name: "Default"},
	}}
	app := newTestApp(r, DefaultSlug)

	resp, body := doRequest(t, app, "missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if body["detail"] != "tenant not found" {
		t.Fatalf("detail = %v, want tenant not found", body["detail"])
	}
}

func TestMiddleware_RequireHeaderWhenDefaultEmpty(t *testing.T) {
	r := fakeResolver{tenants: map[string]Tenant{
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
	app := newTestApp(nil, DefaultSlug)

	resp, body := doRequest(t, app, "anything")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body["id"] != float64(0) || body["slug"] != "" {
		t.Fatalf("locals = %v, want no tenant locals", body)
	}
}

func TestMiddleware_ResolverFailureIsInternalError(t *testing.T) {
	app := newTestApp(fakeResolver{err: context.DeadlineExceeded}, DefaultSlug)

	resp, body := doRequest(t, app, "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if body["detail"] != "failed to resolve tenant" {
		t.Fatalf("detail = %v, want failed to resolve tenant", body["detail"])
	}
}
