package session

import (
	"context"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

type fakeUsers struct {
	repository.UserRepository
	calls int
	user  *sqlc.User
	err   error
}

func (f *fakeUsers) GetByID(_ context.Context, _ int64) (*sqlc.User, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

func TestStore_CachesAndInvalidates(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()

	users := &fakeUsers{user: &sqlc.User{ID: 1, Role: "user", TokenVersion: 0}}
	store := NewStore(users, mem)
	ctx := context.Background()

	if err := store.Validate(ctx, 1, "user", 0); err != nil {
		t.Fatalf("first validate: %v", err)
	}
	if err := store.Validate(ctx, 1, "user", 0); err != nil {
		t.Fatalf("second validate: %v", err)
	}
	if users.calls != 1 {
		t.Fatalf("repository calls = %d, want 1 (second validation should hit cache)", users.calls)
	}

	if err := store.Invalidate(ctx, 1); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if err := store.Validate(ctx, 1, "user", 0); err != nil {
		t.Fatalf("validate after invalidate: %v", err)
	}
	if users.calls != 2 {
		t.Fatalf("repository calls = %d, want 2 after invalidation", users.calls)
	}

	if err := store.Validate(ctx, 1, "admin", 0); err == nil {
		t.Fatal("role mismatch must be rejected")
	}
}

func TestStore_DeletedUserIsUnauthorized(t *testing.T) {
	mem := cache.NewMemoryCache()
	defer func() { _ = mem.Close() }()

	users := &fakeUsers{err: apperror.ErrNotFound}
	store := NewStore(users, mem)
	if err := store.Validate(context.Background(), 1, "user", 0); err == nil {
		t.Fatal("missing user must be rejected")
	}
}

func TestValidateSnapshot(t *testing.T) {
	snap := snapshot{Role: "admin", TokenVersion: 3}

	if err := validateSnapshot(snap, "admin", 3); err != nil {
		t.Fatalf("matching snapshot rejected: %v", err)
	}
	if err := validateSnapshot(snap, "user", 3); err == nil {
		t.Error("role mismatch must be rejected")
	}
	if err := validateSnapshot(snap, "admin", 2); err == nil {
		t.Error("token-version mismatch must be rejected")
	}
}
