package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

// failingSessionCleanup rejects the after-commit cross-tenant session-row
// revocation and counts the attempts.
type failingSessionCleanup struct {
	SessionRepository
	calls int
}

func (f *failingSessionCleanup) RevokeStaleForUser(context.Context, int64, RevokeReason) error {
	f.calls++
	return errors.New("session cleanup unavailable")
}

// The credential change is the business transaction; ending the user's session
// rows in every tenant is a follow-up statement after it commits. A failing
// cleanup must therefore not roll the change back: the token-version bump has
// already invalidated every token and session everywhere.
func TestChangePasswordCommitsWhenSessionCleanupFails(t *testing.T) {
	db := newMemDB()
	stores := db.stores()
	cleanup := &failingSessionCleanup{SessionRepository: stores.Sessions}
	stores.Sessions = cleanup
	users := NewUserService(UserDeps{Stores: stores, Tx: memTx{db}, Cache: cache.NewMemoryCache()})
	ctx := tenant.WithID(context.Background(), tenant.DefaultID)

	currentPassword := strings.Join([]string{"current", "horse", "battery", "staple"}, "-")
	newPassword := strings.Join([]string{"next", "horse", "battery", "staple"}, "-")
	user, err := users.Register(ctx, RegisterInput{
		Email: "cleanup-failure@example.com", Password: currentPassword, Name: "Cleanup",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	err = users.ChangePassword(ctx, user.User.ID, ChangePasswordInput{
		CurrentPassword: currentPassword,
		NewPassword:     newPassword,
	})
	if err != nil {
		t.Fatalf("ChangePassword with a failing cleanup: %v", err)
	}
	if cleanup.calls != 1 {
		t.Fatalf("cleanup attempts = %d, want 1", cleanup.calls)
	}
	if db.users[user.User.ID].TokenVersion == 0 {
		t.Fatal("password change did not bump the token version")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(db.passwords[user.User.ID]), []byte(newPassword)); err != nil {
		t.Fatal("new password was not stored")
	}
}
