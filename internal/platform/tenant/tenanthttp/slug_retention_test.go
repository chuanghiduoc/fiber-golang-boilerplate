package tenanthttp

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// callCountingResolver counts backend lookups so the test can tell cache
// hits from misses.
type callCountingResolver struct {
	tenants map[string]tenant.Tenant
	calls   atomic.Int64
}

func (r *callCountingResolver) Resolve(_ context.Context, slug string) (tenant.Tenant, error) {
	r.calls.Add(1)
	t, ok := r.tenants[slug]
	if !ok {
		return tenant.Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

// TestMiddlewareKeepAliveKeepsCachedSlugsStable reproduces the production
// failure: on one keep-alive connection fasthttp reuses the request buffer, so
// a slug retained without copying is rewritten by the next request.
func TestMiddlewareKeepAliveKeepsCachedSlugsStable(t *testing.T) {
	backend := &callCountingResolver{tenants: map[string]tenant.Tenant{
		"acme": {ID: 2, Slug: "acme"},
		"beta": {ID: 3, Slug: "beta"},
	}}
	cached := tenant.NewCachedResolver(backend, time.Minute, time.Minute, 16)

	app := fiber.New()
	app.Use(Middleware(cached, tenant.DefaultSlug))
	app.Get("/api/v1/ping", func(c fiber.Ctx) error {
		return c.SendString(strconv.FormatInt(ID(c), 10) + ":" + Slug(c))
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	transport := &http.Transport{MaxIdleConnsPerHost: 1}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	want := map[string]string{"acme": "2:acme", "beta": "3:beta"}
	for i, slug := range []string{"acme", "beta", "acme", "beta", "acme"} {
		req, err := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/api/v1/ping", http.NoBody)
		require.NoError(t, err)
		req.Header.Set(tenant.Header, slug)
		resp, err := client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
		assert.Equal(t, want[slug], string(body), "request %d (%s)", i, slug)
	}
	assert.Equal(t, int64(2), backend.calls.Load(), "repeat slugs must hit the cache")

	// The cache keys must still be the original slugs: a key rewritten by a
	// reused request buffer would miss here and reach the backend again.
	for slug, id := range map[string]int64{"acme": 2, "beta": 3} {
		got, err := cached.Resolve(context.Background(), slug)
		require.NoError(t, err)
		assert.Equal(t, id, got.ID, slug)
	}
	assert.Equal(t, int64(2), backend.calls.Load(), "cached keys must be intact")
}
