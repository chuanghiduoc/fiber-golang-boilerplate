package session

import (
	"context"
	"testing"

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
	return State{TenantID: 1, Role: "user", TokenVersion: 0, Active: true, TenantActive: true}
}

func claims() Claims {
	return Claims{UserID: 1, TenantID: 1, SessionID: 7, Role: "user", TokenVersion: 0}
}

func TestStore_CachesAndInvalidates(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()

	states := &fakeStates{state: activeState()}
	store := NewStore(states, mem)
	ctx := context.Background()

	if err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	if err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("second validate: %v", err)
	}
	if states.calls != 1 {
		t.Fatalf("reader calls = %d, want 1 (second validation should hit cache)", states.calls)
	}

	if err := store.Invalidate(ctx, 1); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate after invalidate: %v", err)
	}
	if states.calls != 2 {
		t.Fatalf("reader calls = %d, want 2 after invalidation", states.calls)
	}

	admin := claims()
	admin.Role = "admin"
	if err := store.Validate(ctx, admin); err == nil {
		t.Fatal("role mismatch must be rejected")
	}
}

func TestStore_SnapshotIsPerSession(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()

	states := &fakeStates{state: activeState()}
	store := NewStore(states, mem)
	ctx := context.Background()

	if err := store.Validate(ctx, claims()); err != nil {
		t.Fatalf("validate: %v", err)
	}
	other := claims()
	other.SessionID = 8
	states.state.Active = false // session 8 is revoked
	if err := store.Validate(ctx, other); err == nil {
		t.Fatal("a revoked session must not reuse another session's cached snapshot")
	}
}

func TestStore_UnknownSessionIsUnauthorized(t *testing.T) {
	store := NewStore(&fakeStates{err: apperror.ErrNotFound}, nil)
	if err := store.Validate(context.Background(), claims()); err == nil {
		t.Fatal("unknown session must be rejected")
	}
}

func TestStore_RejectsMissingClaims(t *testing.T) {
	states := &fakeStates{state: activeState()}
	store := NewStore(states, nil)

	noTenant := claims()
	noTenant.TenantID = 0
	if err := store.Validate(context.Background(), noTenant); err == nil {
		t.Fatal("a token without a tenant must be rejected")
	}
	noSession := claims()
	noSession.SessionID = 0
	if err := store.Validate(context.Background(), noSession); err == nil {
		t.Fatal("a token without a session must be rejected")
	}
	if states.calls != 0 {
		t.Fatalf("reader calls = %d, want 0 for malformed claims", states.calls)
	}
}

func TestValidateSnapshot(t *testing.T) {
	snap := snapshot{TenantID: 1, Role: "admin", TokenVersion: 3, Active: true, TenantActive: true}
	ok := Claims{UserID: 1, TenantID: 1, SessionID: 1, Role: "admin", TokenVersion: 3}

	if err := validateSnapshot(snap, ok); err != nil {
		t.Fatalf("matching snapshot rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Claims){
		"role":    func(c *Claims) { c.Role = "user" },
		"version": func(c *Claims) { c.TokenVersion = 2 },
		"tenant":  func(c *Claims) { c.TenantID = 2 },
	} {
		c := ok
		mutate(&c)
		if err := validateSnapshot(snap, c); err == nil {
			t.Errorf("%s mismatch must be rejected", name)
		}
	}

	inactive := snap
	inactive.Active = false
	if err := validateSnapshot(inactive, ok); err == nil {
		t.Error("revoked session / inactive membership must be rejected")
	}
	suspended := snap
	suspended.TenantActive = false
	if err := validateSnapshot(suspended, ok); err == nil {
		t.Error("non-operational tenant must be rejected")
	}
}
