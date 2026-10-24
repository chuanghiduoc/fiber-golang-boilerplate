package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func tenantCtx(id int64) context.Context {
	return tenant.WithID(context.Background(), id)
}

func TestRegister_DuplicateEmailAllowedAcrossTenants(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)

	first, err := svc.Register(tenantCtx(1), dto.RegisterRequest{
		Email: "shared@example.com", Password: "Password1!", Name: "Tenant One",
	})
	if err != nil {
		t.Fatalf("register tenant 1: %v", err)
	}
	second, err := svc.Register(tenantCtx(2), dto.RegisterRequest{
		Email: "shared@example.com", Password: "Password2@", Name: "Tenant Two",
	})
	if err != nil {
		t.Fatalf("same email in another tenant must be allowed: %v", err)
	}
	if first.TenantID != 1 || second.TenantID != 2 {
		t.Fatalf("tenant ids = (%d, %d), want (1, 2)", first.TenantID, second.TenantID)
	}
	if first.ID == second.ID {
		t.Fatal("tenants must not share a user row")
	}

	// The same email is still unique inside one tenant.
	if _, err := svc.Register(tenantCtx(1), dto.RegisterRequest{
		Email: "shared@example.com", Password: "Password3#", Name: "Duplicate",
	}); err == nil {
		t.Fatal("duplicate email inside a tenant must be rejected")
	}
}

func TestAuthenticate_RejectsWrongTenant(t *testing.T) {
	repo := newMockUserRepo()
	svc := newTestUserService(repo, false)

	if _, err := svc.Register(tenantCtx(1), dto.RegisterRequest{
		Email: "tenant1@example.com", Password: "Password1!", Name: "Tenant One",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := svc.Authenticate(tenantCtx(2), dto.LoginRequest{
		Email: "tenant1@example.com", Password: "Password1!",
	}); err == nil {
		t.Fatal("login must fail when the account belongs to another tenant")
	}

	if _, err := svc.Authenticate(tenantCtx(1), dto.LoginRequest{
		Email: "tenant1@example.com", Password: "Password1!",
	}); err != nil {
		t.Fatalf("login in the owning tenant: %v", err)
	}
}

func TestAdminUserService_CrossTenantTargetIsNotFound(t *testing.T) {
	svc, repo, _ := newAdminUserServiceForTest()
	repo.users[1] = &identitysqlc.User{
		TenantID: 2, ID: 1, Email: "tenant2@example.com", Name: "Other", Role: dto.RoleUser,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	ctx := tenantCtx(1)
	assertNotFound := func(err error) {
		t.Helper()
		var appErr *apperror.AppError
		if !errors.As(err, &appErr) || appErr.Status != 404 {
			t.Fatalf("error = %v, want 404 not found", err)
		}
	}
	_, err := svc.UpdateRole(ctx, 1, dto.RoleAdmin)
	assertNotFound(err)
	assertNotFound(svc.BanUser(ctx, 1))
	_, err = svc.UnbanUser(ctx, 1)
	assertNotFound(err)
	users, _, err := svc.ListUsers(ctx, 10, "")
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 0 {
		t.Fatalf("tenant 1 list leaked %d tenant 2 users", len(users))
	}
}

func TestAdminUserService_LastAdminIsPerTenant(t *testing.T) {
	svc, repo, _ := newAdminUserServiceForTest()
	repo.users[1] = &identitysqlc.User{
		TenantID: 1, ID: 1, Email: "admin1@example.com", Name: "Admin One", Role: dto.RoleAdmin,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	repo.users[2] = &identitysqlc.User{
		TenantID: 2, ID: 2, Email: "admin2@example.com", Name: "Admin Two", Role: dto.RoleAdmin,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	repo.nextID = 3

	// Tenant 2's admin must not satisfy tenant 1's last-admin invariant.
	if _, err := svc.UpdateRole(tenantCtx(1), 1, dto.RoleUser); err == nil {
		t.Fatal("demoting the only admin in tenant 1 must fail even though tenant 2 has an admin")
	}

	repo.users[3] = &identitysqlc.User{
		TenantID: 1, ID: 3, Email: "admin1b@example.com", Name: "Admin One B", Role: dto.RoleAdmin,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	repo.nextID = 4

	if _, err := svc.UpdateRole(tenantCtx(1), 1, dto.RoleUser); err != nil {
		t.Fatalf("demoting one of two tenant 1 admins: %v", err)
	}
	if repo.users[2].Role != dto.RoleAdmin {
		t.Fatal("tenant 2 admin must be untouched")
	}
}

func TestRefreshToken_TenantMismatchRejected(t *testing.T) {
	repo := newMockRefreshTokenRepo()
	svc := NewRefreshTokenService(repo, 30)

	plain, err := svc.Create(tenantCtx(1), 7, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, err := svc.Consume(tenantCtx(2), plain); err == nil {
		t.Fatal("refresh token from tenant 1 must not be consumable in tenant 2")
	}
	userID, tenantID, _, err := svc.Consume(tenantCtx(1), plain)
	if err != nil {
		t.Fatalf("consume in owning tenant: %v", err)
	}
	if userID != 7 || tenantID != 1 {
		t.Fatalf("consume = (%d, %d), want (7, 1)", userID, tenantID)
	}
}

func TestEmailVerification_TenantMismatchRejected(t *testing.T) {
	svc, ur, vr, _, _ := newEmailVerifServiceForTest()
	ur.users[1] = &identitysqlc.User{
		TenantID: 1, ID: 1, Email: "user@example.com", Name: "User", Role: dto.RoleUser,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	vr.tokens[hashToken("cross-tenant")] = &identitysqlc.EmailVerificationToken{
		TenantID: 1, ID: 1, UserID: 1, Email: "user@example.com", Token: hashToken("cross-tenant"),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	}

	if err := svc.Verify(tenantCtx(2), "cross-tenant"); err == nil {
		t.Fatal("verification token must not be usable from another tenant")
	}
	if err := svc.Verify(tenantCtx(1), "cross-tenant"); err != nil {
		t.Fatalf("verify in owning tenant: %v", err)
	}
}

func TestEmailVerification_ResendKeepsTenant(t *testing.T) {
	ur := newMockUserRepo()
	vr := newMockEmailVerificationRepo()
	es := newMockEmailSender()
	svc := NewEmailVerificationService(ur, vr, es, newMockCache(), "http://localhost:3000", nil)

	ur.users[1] = &identitysqlc.User{
		TenantID: 2, ID: 1, Email: "tenant2@example.com", Name: "Tenant Two", Role: dto.RoleUser,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	if err := svc.ResendVerification(tenantCtx(2), "tenant2@example.com"); err != nil {
		t.Fatalf("resend: %v", err)
	}
	if !es.WaitFor(1, time.Second) {
		t.Fatal("expected a verification email")
	}
	if len(vr.tokens) != 1 {
		t.Fatalf("verification tokens = %d, want 1", len(vr.tokens))
	}
	for _, tok := range vr.tokens {
		if tok.TenantID != 2 {
			t.Fatalf("verification token tenant = %d, want 2", tok.TenantID)
		}
	}
}

func TestPasswordReset_TenantMismatchRejected(t *testing.T) {
	userRepo := newMockUserRepo()
	resetRepo := newMockPasswordResetRepo()
	svc := newTestPasswordResetService(userRepo, resetRepo, newMockRefreshTokenRepo(), newMockEmailSender(), newMockCache())

	userRepo.users[1] = &identitysqlc.User{
		TenantID: 1, ID: 1, Email: "user@example.com", Name: "User", Role: dto.RoleUser,
		PasswordHash: pgtype.Text{String: "$2a$04$invalid", Valid: true},
		CreatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	resetRepo.tokens[hashToken("cross-tenant")] = &identitysqlc.PasswordResetToken{
		TenantID: 1, ID: 1, UserID: 1, Token: hashToken("cross-tenant"),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	if err := svc.ResetPassword(tenantCtx(2), dto.ResetPasswordRequest{
		Token: "cross-tenant", Password: "NewPassword1!",
	}); err == nil {
		t.Fatal("reset token must not be usable from another tenant")
	}
	if _, ok := resetRepo.tokens[hashToken("cross-tenant")]; !ok {
		t.Fatal("cross-tenant attempt must not consume the token")
	}
}
