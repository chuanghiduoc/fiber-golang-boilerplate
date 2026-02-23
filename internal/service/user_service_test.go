package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func newTestUserService(repo *mockUserRepo, requireEmailVerification bool) UserService {
	return NewUserService(repo, newMockRefreshTokenRepo(), requireEmailVerification, newMockCache(), nil)
}

// ---------------------------------------------------------------------------
// Register
// ---------------------------------------------------------------------------

func TestRegister(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		resp, err := svc.Register(context.Background(), dto.RegisterRequest{
			Email:    "test@example.com",
			Password: "Password1!",
			Name:     "Test User",
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if resp.Email != "test@example.com" {
			t.Errorf("expected email test@example.com, got %s", resp.Email)
		}
		if resp.Name != "Test User" {
			t.Errorf("expected name Test User, got %s", resp.Name)
		}
		if resp.ID != 1 {
			t.Errorf("expected ID 1, got %d", resp.ID)
		}
	})

	t.Run("duplicate email", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		_, err := svc.Register(context.Background(), dto.RegisterRequest{
			Email: "test@example.com", Password: "Password1!", Name: "User1",
		})
		if err != nil {
			t.Fatalf("first register should succeed: %v", err)
		}

		_, err = svc.Register(context.Background(), dto.RegisterRequest{
			Email: "test@example.com", Password: "Password2@", Name: "User2",
		})
		if err == nil {
			t.Fatal("expected error for duplicate email")
		}
		if !strings.Contains(err.Error(), "email already registered") {
			t.Errorf("expected 'email already registered', got %q", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// Authenticate
// ---------------------------------------------------------------------------

func TestAuthenticate(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		_, err := svc.Register(context.Background(), dto.RegisterRequest{
			Email: "test@example.com", Password: "Password1!", Name: "Test User",
		})
		if err != nil {
			t.Fatalf("register: %v", err)
		}

		user, err := svc.Authenticate(context.Background(), dto.LoginRequest{
			Email: "test@example.com", Password: "Password1!",
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if user.Email != "test@example.com" {
			t.Errorf("expected email test@example.com, got %s", user.Email)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		_, _ = svc.Register(context.Background(), dto.RegisterRequest{
			Email: "test@example.com", Password: "Password1!", Name: "Test User",
		})

		_, err := svc.Authenticate(context.Background(), dto.LoginRequest{
			Email: "test@example.com", Password: "WrongPassword2@",
		})
		if err == nil {
			t.Fatal("expected error for wrong password")
		}
		if !strings.Contains(err.Error(), "invalid email or password") {
			t.Errorf("expected 'invalid email or password', got %q", err.Error())
		}
	})

	t.Run("user not found", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		_, err := svc.Authenticate(context.Background(), dto.LoginRequest{
			Email: "nobody@example.com", Password: "Password1!",
		})
		if err == nil {
			t.Fatal("expected error for missing user")
		}
		if !strings.Contains(err.Error(), "invalid email or password") {
			t.Errorf("expected 'invalid email or password', got %q", err.Error())
		}
	})

	t.Run("account locked after max attempts", func(t *testing.T) {
		repo := newMockUserRepo()
		cache := newMockCache()
		svc := NewUserService(repo, newMockRefreshTokenRepo(), false, cache, nil)

		_, _ = svc.Register(context.Background(), dto.RegisterRequest{
			Email: "test@example.com", Password: "Password1!", Name: "Test User",
		})

		// Fail 5 times to trigger lockout
		for i := 0; i < maxLoginAttempts; i++ {
			_, _ = svc.Authenticate(context.Background(), dto.LoginRequest{
				Email: "test@example.com", Password: "Wrong!",
			})
		}

		_, err := svc.Authenticate(context.Background(), dto.LoginRequest{
			Email: "test@example.com", Password: "Password1!",
		})
		if err == nil {
			t.Fatal("expected lockout error")
		}
		if !strings.Contains(err.Error(), "temporarily locked") {
			t.Errorf("expected 'temporarily locked', got %q", err.Error())
		}
	})

	t.Run("email not verified", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, true) // require email verification

		_, _ = svc.Register(context.Background(), dto.RegisterRequest{
			Email: "test@example.com", Password: "Password1!", Name: "Test User",
		})

		_, err := svc.Authenticate(context.Background(), dto.LoginRequest{
			Email: "test@example.com", Password: "Password1!",
		})
		if err == nil {
			t.Fatal("expected error for unverified email")
		}
		if !strings.Contains(err.Error(), "email not verified") {
			t.Errorf("expected 'email not verified', got %q", err.Error())
		}
	})

	t.Run("OAuth account no password hash", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		// User created via OAuth — no password hash
		repo.users[1] = &sqlc.User{
			ID:           1,
			Email:        "oauth@example.com",
			Name:         "OAuth User",
			PasswordHash: pgtype.Text{Valid: false},
			Role:         "user",
		}
		repo.nextID = 2

		_, err := svc.Authenticate(context.Background(), dto.LoginRequest{
			Email: "oauth@example.com", Password: "anything",
		})
		if err == nil {
			t.Fatal("expected error for OAuth account login")
		}
		if !strings.Contains(err.Error(), "invalid email or password") {
			t.Errorf("expected 'invalid email or password', got %q", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// GetByID
// ---------------------------------------------------------------------------

func TestGetByID(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		hash, _ := bcrypt.GenerateFromPassword([]byte("p"), bcrypt.MinCost)
		repo.users[1] = &sqlc.User{
			ID: 1, Email: "test@example.com", Name: "Test",
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			Role:         "user",
		}

		resp, err := svc.GetByID(context.Background(), 1)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if resp.ID != 1 {
			t.Errorf("expected ID 1, got %d", resp.ID)
		}
		if resp.Email != "test@example.com" {
			t.Errorf("expected email test@example.com, got %s", resp.Email)
		}
	})

	t.Run("not found", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		_, err := svc.GetByID(context.Background(), 999)
		if err == nil {
			t.Fatal("expected error for missing user")
		}
		if !strings.Contains(err.Error(), "user not found") {
			t.Errorf("expected 'user not found', got %q", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestList(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		repo.users[1] = &sqlc.User{ID: 1, Email: "a@example.com", Name: "A", Role: "user"}
		repo.users[2] = &sqlc.User{ID: 2, Email: "b@example.com", Name: "B", Role: "user"}
		repo.nextID = 3

		users, total, err := svc.List(context.Background(), 1, 10)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if total != 2 {
			t.Errorf("expected total 2, got %d", total)
		}
		if len(users) != 2 {
			t.Errorf("expected 2 users, got %d", len(users))
		}
	})
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------

func TestUpdate(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		repo.users[1] = &sqlc.User{ID: 1, Email: "old@example.com", Name: "Old Name", Role: "user"}
		repo.nextID = 2

		newName := "New Name"
		resp, err := svc.Update(context.Background(), 1, dto.UpdateUserRequest{Name: &newName})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if resp.Name != "New Name" {
			t.Errorf("expected name 'New Name', got %q", resp.Name)
		}
	})

	t.Run("email conflict", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		repo.users[1] = &sqlc.User{ID: 1, Email: "user1@example.com", Name: "User 1", Role: "user"}
		repo.users[2] = &sqlc.User{ID: 2, Email: "user2@example.com", Name: "User 2", Role: "user"}
		repo.nextID = 3

		taken := "user2@example.com"
		_, err := svc.Update(context.Background(), 1, dto.UpdateUserRequest{Email: &taken})
		if err == nil {
			t.Fatal("expected error for email conflict")
		}
		if !strings.Contains(err.Error(), "email already in use") {
			t.Errorf("expected 'email already in use', got %q", err.Error())
		}
	})

	t.Run("not found", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		name := "X"
		_, err := svc.Update(context.Background(), 999, dto.UpdateUserRequest{Name: &name})
		if err == nil {
			t.Fatal("expected not found error")
		}
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected AppError, got %T", err)
		}
		if appErr.Code != 404 {
			t.Errorf("expected status 404, got %d", appErr.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// Delete
// ---------------------------------------------------------------------------

func TestDelete(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		repo.users[1] = &sqlc.User{ID: 1, Email: "test@example.com", Name: "Test", Role: "user"}

		err := svc.Delete(context.Background(), 1)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(repo.users) != 0 {
			t.Error("expected user to be removed from repo")
		}
	})

	t.Run("not found", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		err := svc.Delete(context.Background(), 999)
		if err == nil {
			t.Fatal("expected not found error")
		}
		if !strings.Contains(err.Error(), "user not found") {
			t.Errorf("expected 'user not found', got %q", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// ChangePassword
// ---------------------------------------------------------------------------

func TestChangePassword(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		hash, _ := bcrypt.GenerateFromPassword([]byte("OldPass1!"), bcrypt.MinCost)
		repo.users[1] = &sqlc.User{
			ID: 1, Email: "test@example.com", Name: "Test", Role: "user",
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		}

		err := svc.ChangePassword(context.Background(), 1, dto.ChangePasswordRequest{
			CurrentPassword: "OldPass1!",
			NewPassword:     "NewPass2@",
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify the password was actually changed
		u := repo.users[1]
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash.String), []byte("NewPass2@")) != nil {
			t.Error("new password hash should match NewPass2@")
		}
	})

	t.Run("wrong current password", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		hash, _ := bcrypt.GenerateFromPassword([]byte("OldPass1!"), bcrypt.MinCost)
		repo.users[1] = &sqlc.User{
			ID: 1, Email: "test@example.com", Name: "Test", Role: "user",
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		}

		err := svc.ChangePassword(context.Background(), 1, dto.ChangePasswordRequest{
			CurrentPassword: "WrongPassword!",
			NewPassword:     "NewPass2@",
		})
		if err == nil {
			t.Fatal("expected error for wrong current password")
		}
		if !strings.Contains(err.Error(), "current password is incorrect") {
			t.Errorf("expected 'current password is incorrect', got %q", err.Error())
		}
	})

	t.Run("OAuth account cannot change password", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		repo.users[1] = &sqlc.User{
			ID: 1, Email: "oauth@example.com", Name: "OAuth", Role: "user",
			PasswordHash: pgtype.Text{Valid: false}, // No password (OAuth)
			AuthProvider: "google",
		}

		err := svc.ChangePassword(context.Background(), 1, dto.ChangePasswordRequest{
			CurrentPassword: "anything",
			NewPassword:     "NewPass2@",
		})
		if err == nil {
			t.Fatal("expected error for OAuth account")
		}
		if !strings.Contains(err.Error(), "cannot change password for OAuth accounts") {
			t.Errorf("expected OAuth error, got %q", err.Error())
		}
	})

	t.Run("user not found", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		err := svc.ChangePassword(context.Background(), 999, dto.ChangePasswordRequest{
			CurrentPassword: "a", NewPassword: "b",
		})
		if err == nil {
			t.Fatal("expected not found error")
		}
		if !strings.Contains(err.Error(), "user not found") {
			t.Errorf("expected 'user not found', got %q", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// FindOrCreateByGoogle
// ---------------------------------------------------------------------------

func TestFindOrCreateByGoogle(t *testing.T) {
	t.Run("existing google user", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		repo.users[1] = &sqlc.User{
			ID: 1, Email: "google@example.com", Name: "Google User",
			GoogleID: pgtype.Text{String: "google-123", Valid: true},
			AuthProvider: "google", Role: "user",
		}
		repo.nextID = 2

		user, err := svc.FindOrCreateByGoogle(context.Background(), "google-123", "google@example.com", "Google User")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if user.ID != 1 {
			t.Errorf("expected ID 1, got %d", user.ID)
		}
	})

	t.Run("link existing email account", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		repo.users[1] = &sqlc.User{
			ID: 1, Email: "existing@example.com", Name: "Existing",
			AuthProvider: "local", Role: "user",
		}
		repo.nextID = 2

		user, err := svc.FindOrCreateByGoogle(context.Background(), "google-456", "existing@example.com", "Existing")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if user.ID != 1 {
			t.Errorf("expected same user ID 1, got %d", user.ID)
		}
		if user.GoogleID.String != "google-456" {
			t.Errorf("expected google ID linked, got %q", user.GoogleID.String)
		}
		if user.AuthProvider != "google" {
			t.Errorf("expected auth_provider 'google', got %q", user.AuthProvider)
		}
	})

	t.Run("create new user", func(t *testing.T) {
		repo := newMockUserRepo()
		svc := newTestUserService(repo, false)

		user, err := svc.FindOrCreateByGoogle(context.Background(), "google-789", "new@example.com", "New User")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if user.Email != "new@example.com" {
			t.Errorf("expected email new@example.com, got %s", user.Email)
		}
		if user.GoogleID.String != "google-789" {
			t.Errorf("expected google ID google-789, got %q", user.GoogleID.String)
		}
		if user.AuthProvider != "google" {
			t.Errorf("expected auth_provider 'google', got %q", user.AuthProvider)
		}
	})
}

// ---------------------------------------------------------------------------
// Update email change success path
// ---------------------------------------------------------------------------

func TestUpdate_EmailChangeSuccess(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)

	repo.users[1] = &sqlc.User{ID: 1, Email: "old@example.com", Name: "User", Role: "user"}
	repo.nextID = 2

	newEmail := "new@example.com"
	resp, err := svc.Update(context.Background(), 1, dto.UpdateUserRequest{Email: &newEmail})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp.Email != "new@example.com" {
		t.Errorf("expected email new@example.com, got %s", resp.Email)
	}
}

func TestUpdate_SameEmail(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)

	repo.users[1] = &sqlc.User{ID: 1, Email: "same@example.com", Name: "User", Role: "user"}
	repo.nextID = 2

	sameEmail := "same@example.com"
	resp, err := svc.Update(context.Background(), 1, dto.UpdateUserRequest{Email: &sameEmail})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp.Email != "same@example.com" {
		t.Errorf("expected email same@example.com, got %s", resp.Email)
	}
}
