package handler

import (
	"context"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// mockSessions is a manual SessionIssuer for handler tests. The refresh
// token "valid-refresh-token" belongs to user 1 in tenant 1.
type mockSessions struct {
	// consumed counts rotations that passed admission (the token was burned).
	consumed int
	revoked  []int64
}

var _ SessionIssuer = (*mockSessions)(nil)

func mockMember(userID int64) *service.Member {
	now := time.Now()
	return &service.Member{
		User: service.User{
			ID: userID, Email: "test@example.com", Name: "Test User",
			Status: service.UserStatusActive, HasPassword: true, CreatedAt: now, UpdatedAt: now,
		},
		Membership: service.Membership{
			TenantID: 1, UserID: userID, Role: "user",
			Status: service.MembershipStatusActive, CreatedAt: now, UpdatedAt: now,
		},
	}
}

func (m *mockSessions) Issue(_ context.Context, _ int64, _ int32, _ []string) (service.TokenPair, error) {
	return service.TokenPair{AccessToken: "mock-access-token", RefreshToken: "mock-refresh-token", SessionID: 1}, nil
}

func (m *mockSessions) Refresh(ctx context.Context, refreshToken string, admit service.Admission) (*service.RefreshResult, error) {
	if refreshToken != "valid-refresh-token" {
		return nil, apperror.NewUnauthorized("invalid refresh token")
	}
	member := mockMember(1)
	if admit != nil {
		if err := admit(ctx, member); err != nil {
			return nil, err
		}
	}
	m.consumed++
	return &service.RefreshResult{
		Tokens: service.TokenPair{AccessToken: "mock-access-token", RefreshToken: "mock-refresh-token-2", SessionID: 1},
		Member: member,
	}, nil
}

func (m *mockSessions) Logout(_ context.Context, _ string) error { return nil }

func (m *mockSessions) List(_ context.Context, userID int64, _ int, _ string) ([]service.Session, bool, error) {
	now := time.Now()
	return []service.Session{
		{ID: 1, TenantID: 1, UserID: userID, UserAgent: "test-agent", IP: "192.0.2.1", CreatedAt: now, LastUsedAt: now},
		{ID: 2, TenantID: 1, UserID: userID, CreatedAt: now.Add(-time.Hour), LastUsedAt: now.Add(-time.Hour)},
	}, false, nil
}

func (m *mockSessions) Revoke(_ context.Context, _, sessionID int64) error {
	if sessionID != 1 && sessionID != 2 {
		return apperror.NewNotFound("session not found")
	}
	m.revoked = append(m.revoked, sessionID)
	return nil
}

// fakeTenantRepository is a TenantLookup over one tenant. Only operational
// (trialing/active) tenants resolve, like the real service.
type fakeTenantRepository struct {
	tenant service.Tenant
	err    error
}

var _ TenantLookup = (*fakeTenantRepository)(nil)

func (f *fakeTenantRepository) ResolveOperational(_ context.Context, slug string) (*service.Tenant, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.tenant.Slug != slug || !f.tenant.Status.Operational() {
		return nil, apperror.ErrNotFound
	}
	t := f.tenant
	return &t, nil
}

func (f *fakeTenantRepository) GetOperational(_ context.Context, id int64) (*service.Tenant, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.tenant.ID != id || !f.tenant.Status.Operational() {
		return nil, apperror.ErrNotFound
	}
	t := f.tenant
	return &t, nil
}

// newTestAuthHandler keeps the handler tests' positional construction
// readable: users, sessions, resets, verification, OAuth state secret,
// (unused) access-token lifetime, the configured email-verification
// requirement, the Google provider, and an optional tenant lookup.
func newTestAuthHandler(
	users service.UserService,
	sessions SessionIssuer,
	resets service.PasswordResetService,
	verification service.EmailVerificationService,
	secret string,
	_ int,
	requireEmailVerification bool,
	google GoogleOAuthProvider,
	tenants ...TenantLookup,
) *AuthHandler {
	deps := AuthDeps{
		Users:                    users,
		Sessions:                 sessions,
		Resets:                   resets,
		Verification:             verification,
		OAuthStateSecret:         secret,
		RequireEmailVerification: requireEmailVerification,
		GoogleOAuth:              google,
	}
	if len(tenants) > 0 {
		deps.Tenants = tenants[0]
	}
	return NewAuthHandler(deps)
}
