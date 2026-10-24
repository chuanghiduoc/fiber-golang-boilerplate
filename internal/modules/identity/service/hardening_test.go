package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

func TestRegister_NormalizesAndDeduplicatesEmail(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)

	resp, err := svc.Register(context.Background(), dto.RegisterRequest{
		Email: "  Test@Example.COM  ", Password: "Password1!", Name: "Test",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.Email != "test@example.com" {
		t.Errorf("email = %q, want normalized lowercase address", resp.Email)
	}

	// Authentication must accept a different casing.
	if _, err := svc.Authenticate(context.Background(), dto.LoginRequest{
		Email: "TEST@EXAMPLE.COM", Password: "Password1!",
	}); err != nil {
		t.Fatalf("login with different casing: %v", err)
	}

	// A second account that differs only by case must be rejected.
	if _, err := svc.Register(context.Background(), dto.RegisterRequest{
		Email: "test@example.com", Password: "Password2@", Name: "Duplicate",
	}); err == nil {
		t.Fatal("expected duplicate-email error")
	}
}

func TestRegister_BannedEmailRejected(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)
	repo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "banned@example.com", Name: "Banned", Role: "user",
		DeletedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		BannedAt:  pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	_, err := svc.Register(context.Background(), dto.RegisterRequest{
		Email: "banned@example.com", Password: "Password1!", Name: "New",
	})
	if err == nil {
		t.Fatal("a banned email must not be reusable")
	}
}

func TestUpdate_EmailChangeResetsVerification(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)
	repo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "old@example.com", Name: "User", Role: "user",
		EmailVerifiedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	newEmail := "new@example.com"
	resp, err := svc.Update(context.Background(), 1, dto.UpdateUserRequest{Email: &newEmail})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if resp.Email != newEmail {
		t.Errorf("email = %q, want %q", resp.Email, newEmail)
	}
	if resp.EmailVerified {
		t.Error("changing email must clear the verified flag")
	}
	if repo.users[1].EmailVerifiedAt.Valid {
		t.Error("stored email_verified_at must be cleared")
	}
}

func TestChangePassword_RevokesSessionsAndBumpsVersion(t *testing.T) {
	repo := newMockUserRepo()
	refreshRepo := newMockRefreshTokenRepo()
	svc := NewUserService(repo, refreshRepo, false, newMockCache(), nil)

	hash, _ := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
	repo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "user@example.com", Name: "User", Role: "user",
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
	}
	refreshRepo.tokens["hash"] = &identitysqlc.RefreshToken{TenantID: 1, UserID: 1, Token: "hash"}

	if err := svc.ChangePassword(context.Background(), 1, dto.ChangePasswordRequest{
		CurrentPassword: "Password1!", NewPassword: "Password2@",
	}); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if len(refreshRepo.tokens) != 0 {
		t.Error("all refresh tokens must be revoked")
	}
	if repo.users[1].TokenVersion == 0 {
		t.Error("token version must be bumped so old access tokens fail")
	}
}

func TestRevokeSessions_InvalidatesAccessAndRefreshTokens(t *testing.T) {
	repo := newMockUserRepo()
	refreshRepo := newMockRefreshTokenRepo()
	svc := NewUserService(repo, refreshRepo, false, newMockCache(), nil)
	repo.users[1] = &identitysqlc.User{TenantID: 1, ID: 1, Email: "user@example.com", Name: "User", Role: "user"}
	refreshRepo.tokens["hash"] = &identitysqlc.RefreshToken{TenantID: 1, UserID: 1, Token: "hash"}

	if err := svc.RevokeSessions(context.Background(), 1); err != nil {
		t.Fatalf("revoke sessions: %v", err)
	}
	if len(refreshRepo.tokens) != 0 {
		t.Error("refresh tokens must be revoked")
	}
	if repo.users[1].TokenVersion == 0 {
		t.Error("token version must be bumped to invalidate access tokens")
	}
}

func TestDelete_CannotDeleteLastAdmin(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)
	repo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "admin@example.com", Name: "Admin", Role: dto.RoleAdmin,
	}

	if err := svc.Delete(context.Background(), 1); err == nil {
		t.Fatal("expected last-admin deletion to be rejected")
	}
}

func TestForgotPassword_NoEnumerationOracle(t *testing.T) {
	userRepo := newMockUserRepo()
	resetRepo := newMockPasswordResetRepo()
	emailSender := newMockEmailSender()
	cache := newMockCache()
	svc := newTestPasswordResetService(userRepo, resetRepo, newMockRefreshTokenRepo(), emailSender, cache)

	hash, _ := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
	userRepo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "known@example.com", Name: "Known", Role: "user",
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
	}

	// First request for a known address sends mail.
	if err := svc.ForgotPassword(context.Background(), dto.ForgotPasswordRequest{Email: "known@example.com"}); err != nil {
		t.Fatalf("known email: %v", err)
	}
	if !emailSender.WaitFor(1, time.Second) {
		t.Fatal("expected reset email")
	}

	// Second request for known and repeated requests for unknown addresses must
	// all look identical: generic success, no mail.
	for _, addr := range []string{"known@example.com", "unknown@example.com", "UNKNOWN@example.com"} {
		if err := svc.ForgotPassword(context.Background(), dto.ForgotPasswordRequest{Email: addr}); err != nil {
			t.Fatalf("%s must be silent, got %v", addr, err)
		}
	}
	if emailSender.Sent() != 1 {
		t.Errorf("rate-limited/unknown addresses must not send mail, sent %d", emailSender.Sent())
	}
}

func TestRefreshToken_ConsumeIsSingleUse(t *testing.T) {
	repo := newMockRefreshTokenRepo()
	svc := NewRefreshTokenService(repo, 30)

	plain, err := svc.Create(context.Background(), 7, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	userID, tenantID, _, err := svc.Consume(context.Background(), plain)
	if err != nil || userID != 7 || tenantID != 1 {
		t.Fatalf("first consume = (%d, %d, %v), want (7, 1, nil)", userID, tenantID, err)
	}
	if _, _, _, err := svc.Consume(context.Background(), plain); err == nil {
		t.Fatal("replayed refresh token must be rejected")
	}
}

func TestEmailVerification_ConsumeIsSingleUse(t *testing.T) {
	svc, ur, vr, _, _ := newEmailVerifServiceForTest()
	ur.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "user@example.com", Name: "User", Role: "user",
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	vr.tokens[hashToken("token")] = &identitysqlc.EmailVerificationToken{TenantID: 1,
		ID: 1, UserID: 1, Email: "user@example.com", Token: hashToken("token"),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}

	if err := svc.Verify(context.Background(), "token"); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if err := svc.Verify(context.Background(), "token"); err == nil {
		t.Fatal("verification token replay must be rejected")
	}
}

func TestRefreshConsumeExpiredTokenIsRejected(t *testing.T) {
	repo := newMockRefreshTokenRepo()
	svc := NewRefreshTokenService(repo, 30)

	// The service hashes the plaintext before looking it up.
	repo.tokens[hashToken("expired")] = &identitysqlc.RefreshToken{TenantID: 1,
		UserID: 1, Token: hashToken("expired"),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	}

	if _, _, _, err := svc.Consume(context.Background(), "expired"); err == nil {
		t.Fatal("expired token must be rejected")
	}
}

func TestResetPassword_RejectsTokenIssuedBeforePasswordChange(t *testing.T) {
	userRepo := newMockUserRepo()
	resetRepo := newMockPasswordResetRepo()
	emailSender := newMockEmailSender()
	cache := newMockCache()
	svc := newTestPasswordResetService(userRepo, resetRepo, newMockRefreshTokenRepo(), emailSender, cache)

	hash, _ := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
	userRepo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "user@example.com", Name: "User", Role: "user",
		PasswordHash:      pgtype.Text{String: string(hash), Valid: true},
		PasswordChangedAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}
	resetRepo.tokens[hashToken("old-token")] = &identitysqlc.PasswordResetToken{TenantID: 1,
		ID: 1, UserID: 1, Token: hashToken("old-token"),
		CreatedAt: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true},
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}

	err := svc.ResetPassword(context.Background(), dto.ResetPasswordRequest{
		Token: "old-token", Password: "NewPassword2@",
	})
	if err == nil {
		t.Fatal("reset token issued before the last password change must be rejected")
	}
}

func TestUpdate_NameOnlyPreservesEmailAndVerification(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)
	repo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "same@example.com", Name: "Old", Role: "user",
		EmailVerifiedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	name := "New Name"
	resp, err := svc.Update(context.Background(), 1, dto.UpdateUserRequest{Name: &name})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if resp.Email != "same@example.com" {
		t.Errorf("email = %q, want unchanged", resp.Email)
	}
	if !resp.EmailVerified {
		t.Error("a name-only update must not clear email verification")
	}
	if repo.users[1].TokenVersion != 0 {
		t.Error("a name-only update must not bump the token version")
	}
}

func TestVerifyPassword_UsesLoginLockout(t *testing.T) {
	repo := newMockUserRepo()
	svc := NewUserService(repo, newMockRefreshTokenRepo(), false, newMockCache(), nil)
	hash, _ := bcrypt.GenerateFromPassword([]byte("Password1!"), bcrypt.MinCost)
	repo.users[1] = &identitysqlc.User{TenantID: 1,
		ID: 1, Email: "user@example.com", Name: "User", Role: "user",
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
	}

	for i := 0; i < maxLoginAttempts; i++ {
		_ = svc.VerifyPassword(context.Background(), 1, "WrongPassword2@")
	}
	if err := svc.VerifyPassword(context.Background(), 1, "Password1!"); err == nil {
		t.Fatal("expected lockout after repeated re-auth failures")
	}
}

func TestUserListCursor_TieBreakerPagesWithoutDuplicates(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)
	now := time.Now()
	for i := int64(1); i <= 3; i++ {
		repo.users[i] = &identitysqlc.User{TenantID: 1,
			ID: i, Email: fmt.Sprintf("u%d@example.com", i), Name: "User", Role: "user",
			CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
			UpdatedAt: pgtype.Timestamptz{Time: now, Valid: true},
		}
	}
	repo.nextID = 4

	page1, hasMore, err := svc.List(context.Background(), 2, "")
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if !hasMore || len(page1) != 2 {
		t.Fatalf("page 1 = %d items, hasMore=%v; want 2,true", len(page1), hasMore)
	}
	cursor := pagination.EncodeCursor(page1[len(page1)-1].CreatedAt, page1[len(page1)-1].ID)

	page2, hasMore, err := svc.List(context.Background(), 2, cursor)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if hasMore || len(page2) != 1 {
		t.Fatalf("page 2 = %d items, hasMore=%v; want 1,false", len(page2), hasMore)
	}
	if page1[0].ID == page2[0].ID || page1[1].ID == page2[0].ID {
		t.Fatal("cursor pagination returned a duplicate row")
	}
}
