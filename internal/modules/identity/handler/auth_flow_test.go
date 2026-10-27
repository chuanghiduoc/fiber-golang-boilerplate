package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
	"github.com/google/uuid"
)

type loginUsers struct {
	service.UserService
	member *service.Member
}

func (u loginUsers) Authenticate(context.Context, service.LoginInput) (*service.Member, error) {
	return u.member, nil
}

type loginMFA struct {
	service.MFAService
}

func (loginMFA) Required(context.Context, uuid.UUID) (bool, error) { return true, nil }

func (loginMFA) BeginChallenge(context.Context, uuid.UUID, string) (string, error) {
	return "challenge-token", nil
}

type loginSessions struct {
	SessionIssuer
	issued bool
}

func (s *loginSessions) Issue(context.Context, uuid.UUID, int32, []string) (service.TokenPair, error) {
	s.issued = true
	return service.TokenPair{AccessToken: "access", RefreshToken: "refresh"}, nil
}

func TestLogin_ConfirmedFactorReturnsChallengeWithoutTokens(t *testing.T) {
	tenantID := testID(1)
	userID := testID(2)
	handler := NewAuthHandler(AuthDeps{
		Users:    loginUsers{member: &service.Member{User: service.User{ID: userID, Email: "mfa@example.com", Name: "MFA", EmailVerifiedAt: timePtr(time.Now())}, Membership: service.Membership{TenantID: tenantID, UserID: userID, Role: service.RoleMember, Status: service.MembershipStatusActive}}},
		Sessions: &loginSessions{},
		MFA:      loginMFA{},
	})

	ctx := tenant.WithID(context.Background(), tenantID)
	out, err := handler.Login(ctx, &loginInput{Body: dto.LoginRequest{Email: "mfa@example.com", Password: handlerTestPassword}})
	require.NoError(t, err)
	assert.True(t, out.Body.MFARequired)
	assert.Equal(t, "challenge-token", out.Body.MFAChallengeToken)
	assert.Empty(t, out.Body.AccessToken)
	assert.Empty(t, out.Body.RefreshToken)
}

func TestLogin_WrongTenantIsInvalidCredentials(t *testing.T) {
	userID := testID(2)
	handler := NewAuthHandler(AuthDeps{
		Users:    loginUsers{member: &service.Member{User: service.User{ID: userID, Email: "x@example.com"}, Membership: service.Membership{TenantID: testID(9), UserID: userID}}},
		Sessions: &loginSessions{},
	})
	_, err := handler.Login(tenant.WithID(context.Background(), testID(1)), &loginInput{
		Body: dto.LoginRequest{Email: "x@example.com", Password: handlerTestPassword},
	})
	require.Error(t, err)
	assert.Equal(t, "auth.invalid_credentials", errorCode(err))
}

type sessionLister struct {
	SessionIssuer
	sessions []service.Session
}

func (s sessionLister) List(context.Context, uuid.UUID, int, string) (listed []service.Session, more bool, listErr error) {
	return s.sessions, false, nil
}

func TestListMySessions_MarksTheCallingSession(t *testing.T) {
	const secret = "session-handler-test-secret-0123456789"
	current := testID(1)
	now := time.Now()
	sessions := sessionLister{sessions: []service.Session{
		{ID: current, UserAgent: "agent", IP: "192.0.2.1", CreatedAt: now, LastUsedAt: now},
		{ID: testID(2), CreatedAt: now.Add(-time.Hour), LastUsedAt: now.Add(-time.Hour)},
	}}
	handler := NewSessionHandler(sessions)
	handler.SetJWTKeyring(token.NewKeyring(secret))
	access, err := token.GenerateForSession(token.SessionClaims{
		UserID: testID(9), TenantID: testID(1), SessionID: current, TokenVersion: 1,
	}, secret, 1)
	require.NoError(t, err)

	out, err := handler.List(context.Background(), &listSessionsInput{
		Limit:         20,
		Authorization: "Bearer " + access,
	})
	require.NoError(t, err)
	require.Len(t, out.Body.Data, 2)
	assert.True(t, out.Body.Data[0].Current)
	assert.False(t, out.Body.Data[1].Current)
	assert.Equal(t, "list", out.Body.Object)
}

func timePtr(t time.Time) *time.Time { return &t }

func errorCode(err error) string {
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return appErr.Code
	}
	return ""
}
