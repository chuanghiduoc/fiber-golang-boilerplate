package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
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
	svc := NewEmailVerificationService(ur, vr, es, mc, "http://localhost:3000", nil).(*emailVerificationService)
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
		if es.Sent() != 1 {
			t.Errorf("expected 1 email sent, got %d", es.Sent())
		}
	})

	t.Run("transactional sender falls back to Send without a tx manager", func(t *testing.T) {
		ur := newMockUserRepo()
		vr := newMockEmailVerificationRepo()
		es := newMockTransactionalEmailSender()
		svc := NewEmailVerificationService(ur, vr, es, newMockCache(), "http://localhost:3000", nil)

		err := svc.SendVerification(context.Background(), 1, "user@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(vr.tokens) != 1 {
			t.Errorf("expected 1 token, got %d", len(vr.tokens))
		}
		if es.Sent() != 1 {
			t.Errorf("expected exactly 1 email sent, got %d", es.Sent())
		}
		if es.SendTxCalls() != 0 {
			t.Errorf("expected no transactional sends without a tx manager, got %d", es.SendTxCalls())
		}
	})

	t.Run("deletes old tokens first", func(t *testing.T) {
		svc, _, vr, _, _ := newEmailVerifServiceForTest()

		// Pre-populate an old token
		vr.tokens["old-token"] = &identitysqlc.EmailVerificationToken{
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

func TestSendVerificationTxRequiresTransaction(t *testing.T) {
	ur := newMockUserRepo()
	vr := newMockEmailVerificationRepo()
	es := newMockTransactionalEmailSender()
	svc := NewEmailVerificationService(ur, vr, es, newMockCache(), "http://localhost:3000", nil)

	err := svc.SendVerificationTx(context.Background(), nil, 1, "user@example.com")
	if err == nil {
		t.Fatal("expected error for nil transaction")
	}
	if len(vr.tokens) != 0 {
		t.Errorf("no token may be created without a transaction, got %d", len(vr.tokens))
	}
	if es.SendTxCalls() != 0 || es.Sent() != 0 {
		t.Errorf("no email may be queued without a transaction, got SendTx=%d Send=%d", es.SendTxCalls(), es.Sent())
	}
}

func TestSendVerificationTxRejectsNonTransactionalSender(t *testing.T) {
	ur := newMockUserRepo()
	vr := newMockEmailVerificationRepo()
	es := newMockEmailSender()
	svc := NewEmailVerificationService(ur, vr, es, newMockCache(), "http://localhost:3000", nil)

	// The sender is checked before the transaction, so a nil tx still proves
	// the capability gate rejects a non-transactional sender.
	err := svc.SendVerificationTx(context.Background(), nil, 1, "user@example.com")
	if err == nil {
		t.Fatal("expected error for a non-transactional sender")
	}
	if !strings.Contains(err.Error(), "transactional") {
		t.Errorf("expected a transactional-sender error, got %q", err.Error())
	}
	if len(vr.tokens) != 0 {
		t.Errorf("no token may be created for a non-transactional sender, got %d", len(vr.tokens))
	}
	if es.Sent() != 0 {
		t.Errorf("no email may be sent for a non-transactional sender, got %d", es.Sent())
	}
}

func TestSupportsTransactionalEnqueue(t *testing.T) {
	t.Run("false without a tx manager", func(t *testing.T) {
		svc := NewEmailVerificationService(
			newMockUserRepo(), newMockEmailVerificationRepo(), newMockTransactionalEmailSender(),
			newMockCache(), "http://localhost:3000", nil,
		)
		if svc.SupportsTransactionalEnqueue() {
			t.Error("a service without a tx manager must not claim transactional enqueue support")
		}
	})

	t.Run("false with a non-transactional sender", func(t *testing.T) {
		svc := NewEmailVerificationService(
			newMockUserRepo(), newMockEmailVerificationRepo(), newMockEmailSender(),
			newMockCache(), "http://localhost:3000", database.NewTxManager(nil),
		)
		if svc.SupportsTransactionalEnqueue() {
			t.Error("a plain sender must not claim transactional enqueue support")
		}
	})

	t.Run("true with a tx manager and transactional sender", func(t *testing.T) {
		svc := NewEmailVerificationService(
			newMockUserRepo(), newMockEmailVerificationRepo(), newMockTransactionalEmailSender(),
			newMockCache(), "http://localhost:3000", database.NewTxManager(nil),
		)
		if !svc.SupportsTransactionalEnqueue() {
			t.Error("a tx manager and transactional sender must support transactional enqueue")
		}
	})
}

func TestVerify(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, vr, _, _ := newEmailVerifServiceForTest()

		// Create a user
		ur.users[1] = &identitysqlc.User{
			ID:        1,
			Email:     "user@example.com",
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}

		// Create a valid token — the repo stores only the hash, so the mock is
		// keyed by hashToken(plaintext) just like the real service writes it.
		vr.tokens[hashToken("valid-token")] = &identitysqlc.EmailVerificationToken{
			ID:        1,
			UserID:    1,
			Email:     "user@example.com",
			Token:     hashToken("valid-token"),
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
		if _, ok := vr.tokens[hashToken("valid-token")]; ok {
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
		if appErr.Status != 400 {
			t.Errorf("status = %d, want 400", appErr.Status)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		svc, _, vr, _, _ := newEmailVerifServiceForTest()

		vr.tokens[hashToken("expired")] = &identitysqlc.EmailVerificationToken{
			ID:        1,
			UserID:    1,
			Email:     "user@example.com",
			Token:     hashToken("expired"),
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
		if appErr.Status != 400 {
			t.Errorf("status = %d, want 400", appErr.Status)
		}

		// Expired token should be deleted
		if _, ok := vr.tokens[hashToken("expired")]; ok {
			t.Error("expired token should be deleted")
		}
	})
}

func TestResendVerification(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		svc, ur, vr, es, _ := newEmailVerifServiceForTest()

		ur.users[1] = &identitysqlc.User{
			ID:        1,
			Email:     "user@example.com",
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}

		err := svc.ResendVerification(context.Background(), "user@example.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !es.WaitFor(1, time.Second) {
			t.Errorf("expected 1 email sent, got %d", es.Sent())
		}
		if len(vr.tokens) != 1 {
			t.Errorf("expected 1 token, got %d", len(vr.tokens))
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		svc, _, _, es, mc := newEmailVerifServiceForTest()

		// Pre-set rate limit cache key
		mc.items["email_verification:user@example.com"] = []byte("1")

		err := svc.ResendVerification(context.Background(), "user@example.com")
		if err != nil {
			t.Fatalf("rate limiting must be silent, got %v", err)
		}
		if es.Sent() != 0 {
			t.Error("rate-limited request must not send email")
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

		ur.users[1] = &identitysqlc.User{
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
		if es.Sent() != 0 {
			t.Errorf("should not send email for already verified user, got %d", es.Sent())
		}
	})
}
