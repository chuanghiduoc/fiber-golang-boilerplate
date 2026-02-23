package service

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func newRefreshTokenServiceForTest() (*refreshTokenService, *mockRefreshTokenRepo) {
	repo := newMockRefreshTokenRepo()
	svc := NewRefreshTokenService(repo, 7).(*refreshTokenService)
	return svc, repo
}

func TestRefreshTokenCreate(t *testing.T) {
	svc, repo := newRefreshTokenServiceForTest()

	plainToken, err := svc.Create(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if plainToken == "" {
		t.Fatal("plainToken should not be empty")
	}

	// Token should be stored as hash, not plaintext
	hashed := hashToken(plainToken)
	if _, ok := repo.tokens[hashed]; !ok {
		t.Error("token should be stored as hash in repo")
	}

	// Plaintext should NOT be in repo
	if _, ok := repo.tokens[plainToken]; ok {
		t.Error("plaintext token should NOT be stored in repo")
	}
}

func TestRefreshTokenVerify(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, repo := newRefreshTokenServiceForTest()

		// Store a hashed token with valid expiry
		plain := "test-token-plain"
		hashed := hashToken(plain)
		repo.tokens[hashed] = &sqlc.RefreshToken{
			ID:        1,
			UserID:    1,
			Token:     hashed,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(7 * 24 * time.Hour), Valid: true},
		}

		rt, err := svc.Verify(context.Background(), plain)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rt.UserID != 1 {
			t.Errorf("UserID = %d, want 1", rt.UserID)
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		svc, _ := newRefreshTokenServiceForTest()

		_, err := svc.Verify(context.Background(), "nonexistent")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		appErr, ok := err.(*apperror.AppError)
		if !ok {
			t.Fatalf("expected *apperror.AppError, got %T", err)
		}
		if appErr.Code != 401 {
			t.Errorf("status = %d, want 401", appErr.Code)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		svc, repo := newRefreshTokenServiceForTest()

		plain := "expired-token"
		hashed := hashToken(plain)
		repo.tokens[hashed] = &sqlc.RefreshToken{
			ID:        1,
			UserID:    1,
			Token:     hashed,
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-1 * time.Hour), Valid: true},
		}

		_, err := svc.Verify(context.Background(), plain)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		appErr, ok := err.(*apperror.AppError)
		if !ok {
			t.Fatalf("expected *apperror.AppError, got %T", err)
		}
		if appErr.Code != 401 {
			t.Errorf("status = %d, want 401", appErr.Code)
		}

		// Expired token should be auto-deleted
		if _, ok := repo.tokens[hashed]; ok {
			t.Error("expired token should be auto-deleted from repo")
		}
	})
}

func TestRefreshTokenRevoke(t *testing.T) {
	svc, repo := newRefreshTokenServiceForTest()

	plain := "revoke-me"
	hashed := hashToken(plain)
	repo.tokens[hashed] = &sqlc.RefreshToken{
		ID:     1,
		UserID: 1,
		Token:  hashed,
	}

	err := svc.Revoke(context.Background(), plain)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := repo.tokens[hashed]; ok {
		t.Error("revoked token should be deleted from repo")
	}
}

func TestRefreshTokenRevokeAllByUserID(t *testing.T) {
	svc, repo := newRefreshTokenServiceForTest()

	// Create multiple tokens for the same user
	repo.tokens["hash1"] = &sqlc.RefreshToken{UserID: 1, Token: "hash1"}
	repo.tokens["hash2"] = &sqlc.RefreshToken{UserID: 1, Token: "hash2"}
	repo.tokens["hash3"] = &sqlc.RefreshToken{UserID: 2, Token: "hash3"}

	err := svc.RevokeAllByUserID(context.Background(), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// User 1 tokens should be deleted
	for k, v := range repo.tokens {
		if v.UserID == 1 {
			t.Errorf("token %q for user 1 should have been deleted", k)
		}
	}

	// User 2 tokens should remain
	if _, ok := repo.tokens["hash3"]; !ok {
		t.Error("user 2 token should not be deleted")
	}
}
