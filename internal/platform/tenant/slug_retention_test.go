package tenant

import (
	"context"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cachedSlugs(r *CachedResolver) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	slugs := make([]string, 0, len(r.entries))
	for slug := range r.entries {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return slugs
}

// TestCachedResolverKeyIsIndependentOfCallerBuffer stores a slug that aliases
// a mutable buffer (as Fiber's zero-copy c.Get does) and then rewrites the
// buffer. The cache key must not change with it.
func TestCachedResolverKeyIsIndependentOfCallerBuffer(t *testing.T) {
	backend := &mapResolver{tenants: map[string]Tenant{
		"acme": {ID: 2, Slug: "acme"},
		"beta": {ID: 3, Slug: "beta"},
	}}
	cached := NewCachedResolver(backend, time.Minute, time.Minute, 16)

	buf := []byte("acme")
	aliased := unsafe.String(&buf[0], len(buf))
	_, err := cached.Resolve(context.Background(), aliased)
	require.NoError(t, err)

	copy(buf, "beta")

	assert.Equal(t, []string{"acme"}, cachedSlugs(cached), "cache key must be an owned copy")
	got, err := cached.Resolve(context.Background(), "acme")
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.ID)
	assert.Equal(t, int64(1), backend.calls.Load(), "the second lookup must be a cache hit")
}

// TestMiddlewareKeepAliveKeepsCachedSlugsStable reproduces the production
// failure: on one keep-alive connection fasthttp reuses the request buffer, so
// a slug retained without copying is rewritten by the next request.
func TestMiddlewareKeepAliveKeepsCachedSlugsStable(t *testing.T) {
	backend := &mapResolver{tenants: map[string]Tenant{
		"acme": {ID: 2, Slug: "acme"},
		"beta": {ID: 3, Slug: "beta"},
	}}
	cached := NewCachedResolver(backend, time.Minute, time.Minute, 16)

	app := fiber.New()
	app.Use(Middleware(cached, DefaultSlug))
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
		req.Header.Set(Header, slug)
		resp, err := client.Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, "request %d", i)
		assert.Equal(t, want[slug], string(body), "request %d (%s)", i, slug)
	}

	assert.Equal(t, []string{"acme", "beta"}, cachedSlugs(cached))
	assert.Equal(t, int64(2), backend.calls.Load(), "repeat slugs must hit the cache")
}
