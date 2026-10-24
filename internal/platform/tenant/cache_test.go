package tenant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type mapResolver struct {
	mu      sync.Mutex
	tenants map[string]Tenant
	err     error
	calls   atomic.Int64
}

func (r *mapResolver) Resolve(_ context.Context, slug string) (Tenant, error) {
	r.calls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return Tenant{}, r.err
	}
	t, ok := r.tenants[slug]
	if !ok {
		return Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

func (r *mapResolver) set(slug string, t *Tenant) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t == nil {
		delete(r.tenants, slug)
		return
	}
	r.tenants[slug] = *t
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newCachedForTest(next Resolver, maxEntries int) (*CachedResolver, *fakeClock) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	r := NewCachedResolver(next, 30*time.Second, 5*time.Second, maxEntries)
	r.now = clock.Now
	return r, clock
}

func TestCachedResolver_CachesHitsUntilTTL(t *testing.T) {
	next := &mapResolver{tenants: map[string]Tenant{"acme": {ID: 7, Slug: "acme"}}}
	r, clock := newCachedForTest(next, 0)
	ctx := context.Background()

	for range 3 {
		got, err := r.Resolve(ctx, "acme")
		if err != nil || got.ID != 7 {
			t.Fatalf("Resolve() = %+v, %v", got, err)
		}
	}
	if n := next.calls.Load(); n != 1 {
		t.Fatalf("wrapped resolver calls = %d, want 1", n)
	}

	// Suspension (resolver now returns not found) takes effect after the TTL.
	next.set("acme", nil)
	clock.Advance(29 * time.Second)
	if _, err := r.Resolve(ctx, "acme"); err != nil {
		t.Fatalf("within TTL the cached tenant must be served, got %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := r.Resolve(ctx, "acme"); !errors.Is(err, apperror.ErrNotFound) {
		t.Fatalf("after TTL a suspended tenant must be rejected, got %v", err)
	}
}

func TestCachedResolver_NegativeCacheIsShort(t *testing.T) {
	next := &mapResolver{tenants: map[string]Tenant{}}
	r, clock := newCachedForTest(next, 0)
	ctx := context.Background()

	for range 3 {
		if _, err := r.Resolve(ctx, "new"); !errors.Is(err, apperror.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if n := next.calls.Load(); n != 1 {
		t.Fatalf("negative result must be cached, calls = %d", n)
	}

	next.set("new", &Tenant{ID: 9, Slug: "new"})
	clock.Advance(6 * time.Second)
	got, err := r.Resolve(ctx, "new")
	if err != nil || got.ID != 9 {
		t.Fatalf("after negative TTL the new tenant must resolve, got %+v, %v", got, err)
	}
}

func TestCachedResolver_DoesNotCacheTransientErrors(t *testing.T) {
	next := &mapResolver{tenants: map[string]Tenant{"acme": {ID: 7, Slug: "acme"}}, err: errors.New("db down")}
	r, _ := newCachedForTest(next, 0)
	ctx := context.Background()

	if _, err := r.Resolve(ctx, "acme"); err == nil {
		t.Fatal("want transient error")
	}
	next.mu.Lock()
	next.err = nil
	next.mu.Unlock()
	got, err := r.Resolve(ctx, "acme")
	if err != nil || got.ID != 7 {
		t.Fatalf("transient error must not be cached, got %+v, %v", got, err)
	}
	if n := next.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2", n)
	}
}

func TestCachedResolver_IsBounded(t *testing.T) {
	next := &mapResolver{tenants: map[string]Tenant{}}
	r, _ := newCachedForTest(next, 4)
	ctx := context.Background()

	for i := range 50 {
		_, _ = r.Resolve(ctx, fmt.Sprintf("slug-%d", i))
	}
	r.mu.Lock()
	size := len(r.entries)
	r.mu.Unlock()
	if size > 4 {
		t.Fatalf("cache size = %d, want <= 4", size)
	}
}

func TestCachedResolver_ConcurrentUse(t *testing.T) {
	tenants := make(map[string]Tenant, 8)
	for i := range 8 {
		slug := fmt.Sprintf("t-%d", i)
		tenants[slug] = Tenant{ID: int64(i + 1), Slug: slug}
	}
	next := &mapResolver{tenants: tenants}
	r := NewCachedResolver(next, time.Minute, time.Second, 4)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 64*100)
	for g := range 64 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 100 {
				slug := fmt.Sprintf("t-%d", (g+i)%10) // t-8/t-9 are unknown
				got, err := r.Resolve(ctx, slug)
				want, known := tenants[slug]
				switch {
				case known && (err != nil || got.ID != want.ID):
					errs <- fmt.Errorf("%s: got %+v, %v", slug, got, err)
				case !known && !errors.Is(err, apperror.ErrNotFound):
					errs <- fmt.Errorf("%s: want not found, got %v", slug, err)
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
