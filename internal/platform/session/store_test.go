package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

type fakeStates struct {
	calls       atomic.Int32
	tenant      uuid.UUID
	state       State
	err         error
	gate        <-chan struct{}
	started     chan<- struct{}
	hasDeadline bool
}

type generationFailingCache struct {
	cache.Cache
	fail atomic.Bool
}

func (c *generationFailingCache) Get(ctx context.Context, key string) ([]byte, error) {
	if c.fail.Load() && strings.HasPrefix(key, "session:generation:") {
		return nil, errors.New("generation store unavailable")
	}
	return c.Cache.Get(ctx, key)
}

type contextCheckingCache struct {
	cache.Cache
	sawCanceled bool
	hasDeadline bool
}

func (c *contextCheckingCache) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	c.sawCanceled = ctx.Err() != nil
	_, c.hasDeadline = ctx.Deadline()
	return c.Cache.Increment(ctx, key, ttl)
}

func (f *fakeStates) SessionState(ctx context.Context, tenantID, _, _ uuid.UUID) (State, error) {
	f.calls.Add(1)
	f.tenant = tenantID
	_, f.hasDeadline = ctx.Deadline()
	if f.started != nil {
		select {
		case f.started <- struct{}{}:
		default:
		}
	}
	if f.gate != nil {
		<-f.gate
	}
	if f.err != nil {
		return State{}, f.err
	}
	return f.state, nil
}

func TestStore_BoundsDetachedStateReads(t *testing.T) {
	states := &fakeStates{state: activeState()}
	store := NewStore(states, nil)

	if _, err := store.Validate(context.Background(), claims()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !states.hasDeadline {
		t.Fatal("detached state read has no deadline; a stalled database can pin the request goroutine")
	}
}

func TestStore_PassesClaimsTenantToReader(t *testing.T) {
	states := &fakeStates{state: activeState()}
	store := NewStore(states, nil)
	ctx := context.Background()

	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if states.tenant != claims().TenantID {
		t.Fatalf("reader tenant = %s, want %s", states.tenant, claims().TenantID)
	}
}

func activeState() State {
	return State{
		TenantID: testID(1), Role: "member", Permissions: []string{"profile:read"},
		TokenVersion: 0, ExpiresAt: time.Now().Add(time.Hour), Active: true, TenantActive: true,
	}
}

func claims() Claims {
	return Claims{UserID: testID(1), TenantID: testID(1), SessionID: testID(7), TokenVersion: 0}
}

func TestStore_CachesAndInvalidates(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()

	states := &fakeStates{state: activeState()}
	store := NewStore(states, mem)
	ctx := context.Background()

	state, err := store.Validate(ctx, claims())
	if err != nil {
		t.Fatalf("first validate: %v", err)
	}
	if len(state.Permissions) != 1 || state.Permissions[0] != "profile:read" {
		t.Fatalf("permissions = %v", state.Permissions)
	}
	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("second validate: %v", err)
	}
	if states.calls.Load() != 1 {
		t.Fatalf("reader calls = %d, want 1 (second validation should hit cache)", states.calls.Load())
	}

	if err := store.Invalidate(ctx, testID(1)); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate after invalidate: %v", err)
	}
	if states.calls.Load() != 2 {
		t.Fatalf("reader calls = %d, want 2 after invalidation", states.calls.Load())
	}

	if err := store.InvalidatePermissions(ctx); err != nil {
		t.Fatalf("invalidate permissions: %v", err)
	}
	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate after permissions invalidate: %v", err)
	}
	if states.calls.Load() != 3 {
		t.Fatalf("reader calls = %d, want 3 after permission invalidation", states.calls.Load())
	}
}

func TestStore_BypassesCacheWhenGenerationIsUnreadable(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()
	partial := &generationFailingCache{Cache: mem}

	states := &fakeStates{state: activeState()}
	store := NewStore(states, partial)
	ctx := context.Background()
	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	if states.calls.Load() != 1 {
		t.Fatalf("reader calls = %d, want 1", states.calls.Load())
	}

	// The cached snapshot still says active. If the generation read fails and
	// the store falls back to generation zero, it would keep authorizing the
	// revoked session for the snapshot TTL. It must read the database instead.
	states.state.Active = false
	partial.fail.Store(true)
	if _, err := store.Validate(ctx, claims()); err == nil {
		t.Fatal("a revoked session must be rejected when the generation store is unavailable")
	}
	if states.calls.Load() != 2 {
		t.Fatalf("reader calls = %d, want a database read while bypassing the cache", states.calls.Load())
	}
}

func TestStore_SnapshotIsPerSession(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()

	states := &fakeStates{state: activeState()}
	store := NewStore(states, mem)
	ctx := context.Background()

	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	other := claims()
	other.SessionID = testID(8)
	states.state.Active = false // session 8 is revoked
	if _, err := store.Validate(ctx, other); err == nil {
		t.Fatal("a revoked session must not reuse another session's cached snapshot")
	}
}

func TestStore_UnknownSessionIsUnauthorized(t *testing.T) {
	store := NewStore(&fakeStates{err: apperror.ErrNotFound}, nil)
	if _, err := store.Validate(context.Background(), claims()); err == nil {
		t.Fatal("unknown session must be rejected")
	}
}

func TestStore_RejectsMissingClaims(t *testing.T) {
	states := &fakeStates{state: activeState()}
	store := NewStore(states, nil)

	noTenant := claims()
	noTenant.TenantID = uuid.Nil
	if _, err := store.Validate(context.Background(), noTenant); err == nil {
		t.Fatal("a token without a tenant must be rejected")
	}
	noSession := claims()
	noSession.SessionID = uuid.Nil
	if _, err := store.Validate(context.Background(), noSession); err == nil {
		t.Fatal("a token without a session must be rejected")
	}
	noUser := claims()
	noUser.UserID = uuid.Nil
	if _, err := store.Validate(context.Background(), noUser); err == nil {
		t.Fatal("a token without a subject must be rejected")
	}
	if states.calls.Load() != 0 {
		t.Fatalf("reader calls = %d, want 0 for malformed claims", states.calls.Load())
	}
}

func TestStore_InvalidateIgnoresCanceledRequestContext(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()
	checking := &contextCheckingCache{Cache: mem}
	states := &fakeStates{state: activeState()}
	store := NewStore(states, checking)

	if _, err := store.Validate(context.Background(), claims()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Invalidate(canceled, claims().UserID); err != nil {
		t.Fatalf("invalidate with canceled request context: %v", err)
	}
	if checking.sawCanceled {
		t.Fatal("cache invalidation inherited the canceled request context")
	}
	if !checking.hasDeadline {
		t.Fatal("cache invalidation has no bounded deadline")
	}
	if _, err := store.Validate(context.Background(), claims()); err != nil {
		t.Fatalf("validate after invalidation: %v", err)
	}
	if got := states.calls.Load(); got != 2 {
		t.Fatalf("reader calls = %d, want 2: invalidation must bump the generation", got)
	}
}

func TestStateOfClonesSnapshotSlices(t *testing.T) {
	snap := snapshot{
		TenantID: testID(1), Role: "member",
		Permissions: []string{"profile:read"}, Platform: false,
		TokenVersion: 0, ExpiresAt: time.Now().Add(time.Hour),
		AMR: []string{"pwd"}, Active: true, TenantActive: true,
	}
	state, err := stateOf(snap, claims())
	if err != nil {
		t.Fatalf("stateOf: %v", err)
	}
	state.Permissions[0] = "admin:all"
	state.AMR[0] = "hwk"
	if snap.Permissions[0] != "profile:read" {
		t.Fatal("stateOf exposed the shared permissions slice")
	}
	if snap.AMR[0] != "pwd" {
		t.Fatal("stateOf exposed the shared AMR slice")
	}
}

func TestStore_CoalescesConcurrentSessionLoads(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	states := &fakeStates{state: activeState(), gate: gate, started: started}
	store := NewStore(states, nil)

	const readers = 12
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Validate(context.Background(), claims())
			errs <- err
		}()
	}

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("session reader did not start")
	}
	// Let the remaining goroutines reach singleflight.Do before the leader
	// completes; otherwise late callers legitimately start a second load.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent validate: %v", err)
		}
	}
	if got := states.calls.Load(); got != 1 {
		t.Fatalf("reader calls = %d, want 1 coalesced load", got)
	}
}

func TestStateOf(t *testing.T) {
	snap := snapshot{TenantID: testID(1), Role: "admin", Permissions: []string{"admin:users:read"},
		TokenVersion: 3, ExpiresAt: time.Now().Add(time.Hour), Active: true, TenantActive: true}
	ok := Claims{UserID: testID(1), TenantID: testID(1), SessionID: testID(1), TokenVersion: 3}

	state, err := stateOf(snap, ok)
	if err != nil {
		t.Fatalf("matching snapshot rejected: %v", err)
	}
	if state.Role != "admin" || len(state.Permissions) != 1 {
		t.Fatalf("state = %+v", state)
	}
	for name, mutate := range map[string]func(*Claims){
		"version": func(c *Claims) { c.TokenVersion = 2 },
		"tenant":  func(c *Claims) { c.TenantID = testID(2) },
	} {
		c := ok
		mutate(&c)
		if _, err := stateOf(snap, c); err == nil {
			t.Errorf("%s mismatch must be rejected", name)
		}
	}

	inactive := snap
	inactive.Active = false
	if _, err := stateOf(inactive, ok); err == nil {
		t.Error("revoked session / inactive membership must be rejected")
	}
	suspended := snap
	suspended.TenantActive = false
	if _, err := stateOf(suspended, ok); err == nil {
		t.Error("non-operational tenant must be rejected")
	}
	expired := snap
	expired.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := stateOf(expired, ok); err == nil {
		t.Error("an expired session must be rejected")
	}
	noExpiry := snap
	noExpiry.ExpiresAt = time.Time{}
	if _, err := stateOf(noExpiry, ok); err == nil {
		t.Error("a snapshot without an expiry must fail closed")
	}
}
