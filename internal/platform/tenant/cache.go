package tenant

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Defaults for NewCachedResolver. Every /api/v1 request resolves its tenant
// before any rate limiter runs, so an uncached resolver turns each request
// (including 404s) into a tenants query.
//
// A tenant suspended or deleted in the database keeps resolving from this
// process for at most DefaultCacheTTL; a newly created or reactivated slug
// becomes visible within DefaultNegativeCacheTTL. Authenticated requests are
// also checked against the stored tenant status by the session validator and
// the API-key verifier, independently of this cache.
const (
	DefaultCacheTTL         = 30 * time.Second
	DefaultNegativeCacheTTL = 5 * time.Second
	DefaultCacheMaxEntries  = 1024
)

// CachedResolver is a bounded, in-process TTL cache in front of a Resolver.
// Successful resolutions are cached for ttl and not-found results for
// negativeTTL; any other error is never cached so a transient database failure
// is retried on the next request. It is safe for concurrent use.
type CachedResolver struct {
	next        Resolver
	ttl         time.Duration
	negativeTTL time.Duration
	maxEntries  int
	now         func() time.Time

	mu      sync.Mutex
	entries map[string]cachedTenant
}

type cachedTenant struct {
	tenant    Tenant
	notFound  bool
	expiresAt time.Time
}

var _ Resolver = (*CachedResolver)(nil)

// NewCachedResolver wraps next. Non-positive ttl/negativeTTL disable caching
// of the corresponding result; a non-positive maxEntries uses
// DefaultCacheMaxEntries.
func NewCachedResolver(next Resolver, ttl, negativeTTL time.Duration, maxEntries int) *CachedResolver {
	if maxEntries <= 0 {
		maxEntries = DefaultCacheMaxEntries
	}
	return &CachedResolver{
		next:        next,
		ttl:         ttl,
		negativeTTL: negativeTTL,
		maxEntries:  maxEntries,
		now:         time.Now,
		entries:     make(map[string]cachedTenant),
	}
}

// Resolve returns the cached resolution for slug or delegates to the wrapped
// resolver. Concurrent misses for the same slug may each reach the wrapped
// resolver; the cache only bounds steady-state load.
func (r *CachedResolver) Resolve(ctx context.Context, slug string) (Tenant, error) {
	now := r.now()

	r.mu.Lock()
	entry, ok := r.entries[slug]
	if ok && now.Before(entry.expiresAt) {
		r.mu.Unlock()
		if entry.notFound {
			return Tenant{}, apperror.ErrNotFound
		}
		return entry.tenant, nil
	}
	r.mu.Unlock()

	resolved, err := r.next.Resolve(ctx, slug)
	switch {
	case err == nil:
		r.store(slug, cachedTenant{tenant: resolved}, r.ttl)
	case errors.Is(err, apperror.ErrNotFound):
		r.store(slug, cachedTenant{notFound: true}, r.negativeTTL)
	}
	return resolved, err
}

func (r *CachedResolver) store(slug string, entry cachedTenant, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	now := r.now()
	entry.expiresAt = now.Add(ttl)

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[slug]; !exists && len(r.entries) >= r.maxEntries {
		r.evictLocked(now)
	}
	r.entries[slug] = entry
}

// evictLocked drops expired entries and, if the cache is still full, one
// arbitrary entry. Random slugs (for example a 404 scan) can therefore only
// displace entries, never grow the map beyond maxEntries.
func (r *CachedResolver) evictLocked(now time.Time) {
	for slug, entry := range r.entries {
		if !now.Before(entry.expiresAt) {
			delete(r.entries, slug)
		}
	}
	if len(r.entries) < r.maxEntries {
		return
	}
	for slug := range r.entries {
		delete(r.entries, slug)
		return
	}
}
