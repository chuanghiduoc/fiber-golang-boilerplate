package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

func newTestPasswordResetService(
	userRepo *mockUserRepo,
	resetRepo *mockPasswordResetRepo,
	refreshRepo *mockRefreshTokenRepo,
	emailSender *mockEmailSender,
	cache *mockCache,
) PasswordResetService {
	return NewPasswordResetService(
		userRepo, resetRepo, refreshRepo,
		emailSender, cache,
		"http://localhost:3000",
		nil, // no txManager for tests
	)
}

// ---------------------------------------------------------------------------
// ForgotPassword
// ---------------------------------------------------------------------------

func TestForgotPassword(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		userRepo := newMockUserRepo()
		resetRepo := newMockPasswordResetRepo()
		refreshRepo := newMockRefreshTokenRepo()
		emailSender := newMockEmailSender()
		cache := newMockCache()
		svc := newTestPasswordResetService(userRepo, resetRepo, refreshRepo, emailSender, cache)

		hash, _ := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
		userRepo.users[1] = &sqlc.User{
			ID: 1, Email: "test@example.com", Name: "Test",
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			Role:         "user",
		}

		err := svc.ForgotPassword(context.Background(), dto.ForgotPasswordRequest{
			Email: "test@example.com",
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify a token was created
		if len(resetRepo.tokens) != 1 {
			t.Errorf("expected 1 reset token, got %d", len(resetRepo.tokens))
		}

		// Verify email was sent
		if emailSender.sent != 1 {
			t.Errorf("expected 1 email sent, got %d", emailSender.sent)
		}

		// Verify rate limit was set
		if _, ok := cache.items["password_reset:test@example.com"]; !ok {
			t.Error("expected rate limit cache key to be set")
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		userRepo := newMockUserRepo()
		resetRepo := newMockPasswordResetRepo()
		refreshRepo := newMockRefreshTokenRepo()
		emailSender := newMockEmailSender()
		cache := newMockCache()
		svc := newTestPasswordResetService(userRepo, resetRepo, refreshRepo, emailSender, cache)

		// Pre-set rate limit
		cache.items["password_reset:test@example.com"] = []byte("1")

		err := svc.ForgotPassword(context.Background(), dto.ForgotPasswordRequest{
			Email: "test@example.com",
		})
		if err == nil {
			t.Fatal("expected rate limit error")
		}
		if !strings.Contains(err.Error(), "please wait") {
			t.Errorf("expected rate limit message, got %q", err.Error())
		}
	})

	t.Run("user not found returns nil (silent fail)", func(t *testing.T) {
		userRepo := newMockUserRepo()
		resetRepo := newMockPasswordResetRepo()
		refreshRepo := newMockRefreshTokenRepo()
		emailSender := newMockEmailSender()
		cache := newMockCache()
		svc := newTestPasswordResetService(userRepo, resetRepo, refreshRepo, emailSender, cache)

		err := svc.ForgotPassword(context.Background(), dto.ForgotPasswordRequest{
			Email: "nobody@example.com",
		})
		if err != nil {
			t.Fatalf("expected nil (silent fail for unknown email), got %v", err)
		}

		// No token should be created
		if len(resetRepo.tokens) != 0 {
			t.Errorf("expected 0 reset tokens, got %d", len(resetRepo.tokens))
		}

		// No email should be sent
		if emailSender.sent != 0 {
			t.Errorf("expected 0 emails, got %d", emailSender.sent)
		}
	})
}

// ---------------------------------------------------------------------------
// ResetPassword
// ---------------------------------------------------------------------------

func TestResetPassword(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		userRepo := newMockUserRepo()
		resetRepo := newMockPasswordResetRepo()
		refreshRepo := newMockRefreshTokenRepo()
		emailSender := newMockEmailSender()
		cache := newMockCache()
		svc := newTestPasswordResetService(userRepo, resetRepo, refreshRepo, emailSender, cache)

		hash, _ := bcrypt.GenerateFromPassword([]byte("OldPass1!"), bcrypt.MinCost)
		userRepo.users[1] = &sqlc.User{
			ID: 1, Email: "test@example.com", Name: "Test",
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			Role:         "user",
		}

		// Create a valid reset token
		resetRepo.tokens["valid-token"] = &sqlc.PasswordResetToken{
			ID:        1,
			UserID:    1,
			Token:     "valid-token",
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(1 * time.Hour), Valid: true},
		}

		err := svc.ResetPassword(context.Background(), dto.ResetPasswordRequest{
			Token:    "valid-token",
			Password: "NewPass2@",
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify password was updated
		u := userRepo.users[1]
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash.String), []byte("NewPass2@")) != nil {
			t.Error("password hash should match NewPass2@")
		}

		// Verify token was cleaned up
		if len(resetRepo.tokens) != 0 {
			t.Errorf("expected reset token to be deleted, got %d tokens", len(resetRepo.tokens))
		}
	})

	t.Run("expired token", func(t *testing.T) {
		userRepo := newMockUserRepo()
		resetRepo := newMockPasswordResetRepo()
		refreshRepo := newMockRefreshTokenRepo()
		emailSender := newMockEmailSender()
		cache := newMockCache()
		svc := newTestPasswordResetService(userRepo, resetRepo, refreshRepo, emailSender, cache)

		// Expired token
		resetRepo.tokens["expired-token"] = &sqlc.PasswordResetToken{
			ID:        1,
			UserID:    1,
			Token:     "expired-token",
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true},
		}

		err := svc.ResetPassword(context.Background(), dto.ResetPasswordRequest{
			Token:    "expired-token",
			Password: "NewPass2@",
		})
		if err == nil {
			t.Fatal("expected error for expired token")
		}
		if !strings.Contains(err.Error(), "expired") {
			t.Errorf("expected 'expired' in error, got %q", err.Error())
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		userRepo := newMockUserRepo()
		resetRepo := newMockPasswordResetRepo()
		refreshRepo := newMockRefreshTokenRepo()
		emailSender := newMockEmailSender()
		cache := newMockCache()
		svc := newTestPasswordResetService(userRepo, resetRepo, refreshRepo, emailSender, cache)

		err := svc.ResetPassword(context.Background(), dto.ResetPasswordRequest{
			Token:    "nonexistent-token",
			Password: "NewPass2@",
		})
		if err == nil {
			t.Fatal("expected error for invalid token")
		}
		if !strings.Contains(err.Error(), "invalid or expired reset token") {
			t.Errorf("expected 'invalid or expired reset token', got %q", err.Error())
		}
	})

	t.Run("revokes all refresh tokens", func(t *testing.T) {
		userRepo := newMockUserRepo()
		resetRepo := newMockPasswordResetRepo()
		refreshRepo := newMockRefreshTokenRepo()
		emailSender := newMockEmailSender()
		cache := newMockCache()
		svc := newTestPasswordResetService(userRepo, resetRepo, refreshRepo, emailSender, cache)

		hash, _ := bcrypt.GenerateFromPassword([]byte("OldPass1!"), bcrypt.MinCost)
		userRepo.users[1] = &sqlc.User{
			ID: 1, Email: "test@example.com", Name: "Test",
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			Role:         "user",
		}

		// Add some refresh tokens for the user
		refreshRepo.tokens["rt1"] = &sqlc.RefreshToken{UserID: 1, Token: "rt1"}
		refreshRepo.tokens["rt2"] = &sqlc.RefreshToken{UserID: 1, Token: "rt2"}

		resetRepo.tokens["valid-token"] = &sqlc.PasswordResetToken{
			ID:        1,
			UserID:    1,
			Token:     "valid-token",
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(1 * time.Hour), Valid: true},
		}

		err := svc.ResetPassword(context.Background(), dto.ResetPasswordRequest{
			Token:    "valid-token",
			Password: "NewPass2@",
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// All refresh tokens for user should be revoked
		if len(refreshRepo.tokens) != 0 {
			t.Errorf("expected all refresh tokens to be revoked, got %d", len(refreshRepo.tokens))
		}
	})
}
