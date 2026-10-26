package session

import (
	"context"
	"testing"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

type fakeStates struct {
	calls int
	state State
	err   error
}

func (f *fakeStates) SessionState(_ context.Context, _, _ int64) (State, error) {
	f.calls++
	if f.err != nil {
		return State{}, f.err
	}
	return f.state, nil
}

func activeState() State {
	return State{
		TenantID: 1, Role: "member", Permissions: []string{"profile:read"},
		TokenVersion: 0, ExpiresAt: time.Now().Add(time.Hour), Active: true, TenantActive: true,
	}
}

func claims() Claims {
	return Claims{UserID: 1, TenantID: 1, SessionID: 7, TokenVersion: 0}
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
	if states.calls != 1 {
		t.Fatalf("reader calls = %d, want 1 (second validation should hit cache)", states.calls)
	}

	if err := store.Invalidate(ctx, 1); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate after invalidate: %v", err)
	}
	if states.calls != 2 {
		t.Fatalf("reader calls = %d, want 2 after invalidation", states.calls)
	}

	if err := store.InvalidatePermissions(ctx); err != nil {
		t.Fatalf("invalidate permissions: %v", err)
	}
	if _, err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate after permissions invalidate: %v", err)
	}
	if states.calls != 3 {
		t.Fatalf("reader calls = %d, want 3 after permission invalidation", states.calls)
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
	other.SessionID = 8
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
	noTenant.TenantID = 0
	if _, err := store.Validate(context.Background(), noTenant); err == nil {
		t.Fatal("a token without a tenant must be rejected")
	}
	noSession := claims()
	noSession.SessionID = 0
	if _, err := store.Validate(context.Background(), noSession); err == nil {
		t.Fatal("a token without a session must be rejected")
	}
	if states.calls != 0 {
		t.Fatalf("reader calls = %d, want 0 for malformed claims", states.calls)
	}
}

func TestStateOf(t *testing.T) {
	snap := snapshot{TenantID: 1, Role: "admin", Permissions: []string{"admin:users:read"},
		TokenVersion: 3, ExpiresAt: time.Now().Add(time.Hour), Active: true, TenantActive: true}
	ok := Claims{UserID: 1, TenantID: 1, SessionID: 1, TokenVersion: 3}

	state, err := stateOf(snap, ok)
	if err != nil {
		t.Fatalf("matching snapshot rejected: %v", err)
	}
	if state.Role != "admin" || len(state.Permissions) != 1 {
		t.Fatalf("state = %+v", state)
	}
	for name, mutate := range map[string]func(*Claims){
		"version": func(c *Claims) { c.TokenVersion = 2 },
		"tenant":  func(c *Claims) { c.TenantID = 2 },
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
