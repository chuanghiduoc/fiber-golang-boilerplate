package tenant

import (
	"context"
	"slices"
	"testing"
	"time"
	"unsafe"

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
	slices.Sort(slugs)
	return slugs
}

// TestCachedResolverKeyIsIndependentOfCallerBuffer stores a slug that aliases
// a mutable buffer (as Fiber's zero-copy c.Get does) and then rewrites the
// buffer. The cache key must not change with it.
func TestCachedResolverKeyIsIndependentOfCallerBuffer(t *testing.T) {
	backend := &mapResolver{tenants: map[string]Tenant{
		"acme": {ID: testID(2), Slug: "acme"},
		"beta": {ID: testID(3), Slug: "beta"},
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
	assert.Equal(t, testID(2), got.ID)
	assert.Equal(t, int64(1), backend.calls.Load(), "the second lookup must be a cache hit")
}
