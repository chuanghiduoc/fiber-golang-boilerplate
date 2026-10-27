package authn

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/google/uuid"
)

// failingSessionCleanup rejects the after-commit cross-tenant session-row
// revocation and counts the attempts.
type failingSessionCleanup struct {
	core.SessionRepository
	calls int
}

func (f *failingSessionCleanup) RevokeStaleForUser(context.Context, uuid.UUID, core.RevokeReason) error {
	f.calls++
	return errors.New("session cleanup unavailable")
}

// The credential change is the business transaction; ending the user's session
// rows in every tenant is a follow-up statement after it commits. A failing
// cleanup must therefore not roll the change back: the token-version bump has
// already invalidated every token and session everywhere.
func TestChangePasswordCommitsWhenSessionCleanupFails(t *testing.T) {
	db := fakes.New()
	stores := db.Stores()
	cleanup := &failingSessionCleanup{SessionRepository: stores.Sessions}
	stores.Sessions = cleanup
	users := NewUserService(UserDeps{Stores: stores, Tx: fakes.Tx{DB: db}, Cache: cache.NewMemoryCache()})
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
	if db.Users[user.User.ID].TokenVersion == 0 {
		t.Fatal("password change did not bump the token version")
	}
	if ok, _ := verifyPassword(db.Passwords[user.User.ID], newPassword); !ok {
		t.Fatal("new password was not stored")
	}
}
