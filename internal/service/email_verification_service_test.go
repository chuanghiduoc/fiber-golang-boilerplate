package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func newEmailVerifServiceForTest() (
	*emailVerificationService,
	*mockUserRepo,
	*mockEmailVerificationRepo,
	*mockEmailSender,
	*mockCache,
) {
	ur := newMockUserRepo()
	vr := newMockEmailVerificationRepo()
	es := newMockEmailSender()
	mc := newMockCache()
	svc := NewEmailVerificationService(ur, vr, es, mc, "http://localhost:3000").(*emailVerificationService)
	return svc, ur, vr, es, mc
}

func TestSendVerification(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, _, vr, es, _ := newEmailVerifServiceForTest()

		err := svc.SendVerification(context.Background(), 1, "user@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Token should be created
		if len(vr.tokens) != 1 {
			t.Errorf("expected 1 token, got %d", len(vr.tokens))
		}

		// Email should be sent
		if es.sent != 1 {
			t.Errorf("expected 1 email sent, got %d", es.sent)
		}
	})

	t.Run("deletes old tokens first", func(t *testing.T) {
		svc, _, vr, _, _ := newEmailVerifServiceForTest()

		// Pre-populate an old token
		vr.tokens["old-token"] = &sqlc.EmailVerificationToken{
			ID:     1,
			UserID: 1,
			Token:  "old-token",
		}

		err := svc.SendVerification(context.Background(), 1, "user@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Old token should be deleted, only new one should remain
		if _, ok := vr.tokens["old-token"]; ok {
			t.Error("old token should have been deleted")
		}
		if len(vr.tokens) != 1 {
			t.Errorf("expected 1 token, got %d", len(vr.tokens))
		}
	})
}

func TestVerify(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, vr, _, _ := newEmailVerifServiceForTest()

		// Create a user
		ur.users[1] = &sqlc.User{
			ID:        1,
			Email:     "user@example.com",
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}

		// Create a valid token
		vr.tokens["valid-token"] = &sqlc.EmailVerificationToken{
			ID:        1,
			UserID:    1,
			Token:     "valid-token",
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
		}

		err := svc.Verify(context.Background(), "valid-token")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// User should be verified
		if !ur.users[1].EmailVerifiedAt.Valid {
			t.Error("user email should be verified")
		}

		// Token should be deleted
		if _, ok := vr.tokens["valid-token"]; ok {
			t.Error("token should be deleted after verification")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		svc, _, _, _, _ := newEmailVerifServiceForTest()

		err := svc.Verify(context.Background(), "nonexistent")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		appErr, ok := err.(*apperror.AppError)
		if !ok {
			t.Fatalf("expected *apperror.AppError, got %T", err)
		}
		if appErr.Code != 400 {
			t.Errorf("status = %d, want 400", appErr.Code)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		svc, _, vr, _, _ := newEmailVerifServiceForTest()

		vr.tokens["expired"] = &sqlc.EmailVerificationToken{
			ID:        1,
			UserID:    1,
			Token:     "expired",
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true},
		}

		err := svc.Verify(context.Background(), "expired")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		appErr, ok := err.(*apperror.AppError)
		if !ok {
			t.Fatalf("expected *apperror.AppError, got %T", err)
		}
		if appErr.Code != 400 {
			t.Errorf("status = %d, want 400", appErr.Code)
		}

		// Expired token should be deleted
		if _, ok := vr.tokens["expired"]; ok {
			t.Error("expired token should be deleted")
		}
	})
}

func TestResendVerification(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, vr, es, _ := newEmailVerifServiceForTest()

		ur.users[1] = &sqlc.User{
			ID:        1,
			Email:     "user@example.com",
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}

		err := svc.ResendVerification(context.Background(), "user@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if es.sent != 1 {
			t.Errorf("expected 1 email sent, got %d", es.sent)
		}
		if len(vr.tokens) != 1 {
			t.Errorf("expected 1 token, got %d", len(vr.tokens))
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		svc, _, _, _, mc := newEmailVerifServiceForTest()

		// Pre-set rate limit cache key
		mc.items["email_verification:user@example.com"] = []byte("1")

		err := svc.ResendVerification(context.Background(), "user@example.com")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		appErr, ok := err.(*apperror.AppError)
		if !ok {
			t.Fatalf("expected *apperror.AppError, got %T", err)
		}
		if appErr.Code != 400 {
			t.Errorf("status = %d, want 400", appErr.Code)
		}
	})

	t.Run("user not found returns nil", func(t *testing.T) {
		svc, _, _, _, _ := newEmailVerifServiceForTest()

		err := svc.ResendVerification(context.Background(), "nobody@example.com")
		if err != nil {
			t.Fatalf("expected nil for user not found, got: %v", err)
		}
	})

	t.Run("already verified returns nil", func(t *testing.T) {
		svc, ur, _, es, _ := newEmailVerifServiceForTest()

		ur.users[1] = &sqlc.User{
			ID:              1,
			Email:           "user@example.com",
			EmailVerifiedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			CreatedAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}

		err := svc.ResendVerification(context.Background(), "user@example.com")
		if err != nil {
			t.Fatalf("expected nil for already verified, got: %v", err)
		}
		if es.sent != 0 {
			t.Errorf("should not send email for already verified user, got %d", es.sent)
		}
	})
}
