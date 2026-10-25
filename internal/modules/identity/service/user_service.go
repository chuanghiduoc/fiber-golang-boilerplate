package service

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
)

// bcryptCost is the production password-hashing cost. It is a variable only
// so this package's tests can lower it in TestMain (main_test.go).
var bcryptCost = 12

// UserService owns the account lifecycle as seen from the request tenant.
// Users are global identities (ADR 0004); every read here goes through the
// caller's membership in the request tenant. Its methods are split by
// responsibility across this package: registration.go, authentication.go and
// lockout.go, oauth_google.go, profile.go (update/delete/sessions), and
// password.go.
type UserService interface {
	Register(ctx context.Context, req RegisterInput) (*Member, error)
	// Authenticate checks the global password and requires an active
	// membership in the request tenant.
	Authenticate(ctx context.Context, req LoginInput) (*Member, error)
	// SignInWithGoogle signs in an existing or linkable account, or creates
	// one when req.AllowSignUp is set (see GoogleSignIn), and issues the
	// session in the same transaction as the auth.google_login audit row.
	SignInWithGoogle(ctx context.Context, req GoogleSignIn) (*GoogleSignInResult, error)
	// GetByID returns an active member of the request tenant.
	GetByID(ctx context.Context, id int64) (*Member, error)
	List(ctx context.Context, limit int, startingAfter string) ([]Member, bool, error)
	// Update changes the caller's own global profile.
	Update(ctx context.Context, id int64, req UpdateProfileInput) (*Member, error)
	VerifyPassword(ctx context.Context, userID int64, password string) error
	// DeleteAccount deletes the caller's global account (self-service only).
	DeleteAccount(ctx context.Context, id int64) error
	// RemoveMember removes another user's membership from the request tenant.
	RemoveMember(ctx context.Context, id int64) error
	ChangePassword(ctx context.Context, userID int64, req ChangePasswordInput) error
	// RevokeSessions ends every session of the user in every tenant
	// (logout-all) and invalidates every access token.
	RevokeSessions(ctx context.Context, userID int64) error
	// RegistrationEmailsTransactional reports whether Register commits the
	// verification email with the user. Callers must not send a second email
	// when it returns true.
	RegistrationEmailsTransactional() bool
}

// UserDeps are the user service's collaborators.
type UserDeps struct {
	Stores Stores
	Tx     Transactor
	Cache  cache.Cache
	// SessionCache is the access-token validator; nil disables invalidation.
	SessionCache session.Store
	// Sessions issues sessions for Google sign-in.
	Sessions *SessionService
	// Verification provisions the registration email. When it can enqueue
	// on the registration transaction (MailTransactional), the user, token,
	// and email job commit together; otherwise the handler sends after
	// commit.
	Verification             EmailVerificationService
	MailTransactional        bool
	RequireEmailVerification bool
}

type userService struct {
	UserDeps
}

// NewUserService wires the user service.
func NewUserService(deps UserDeps) UserService {
	return &userService{UserDeps: deps}
}

func (s *userService) RegistrationEmailsTransactional() bool {
	_, ok := s.Verification.(txProvisioner)
	return ok && s.MailTransactional
}

// getMember loads an active member of the request tenant as a public error.
func getMember(ctx context.Context, memberships MembershipRepository, id int64) (*Member, error) {
	member, err := memberships.GetActive(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
	}
	return member, nil
}

func (s *userService) GetByID(ctx context.Context, id int64) (*Member, error) {
	return getMember(ctx, s.Stores.Memberships, id)
}

func (s *userService) List(ctx context.Context, limit int, startingAfter string) ([]Member, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	members, err := s.Stores.Memberships.ListActiveCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list users")
	}
	return memberPage(members, pageSize)
}

// memberPage trims the extra probe row.
func memberPage(members []Member, pageSize int) ([]Member, bool, error) {
	hasMore := len(members) > pageSize
	if hasMore {
		members = members[:pageSize]
	}
	return members, hasMore, nil
}

// userWriteError maps a failed write: a missing row becomes 404 with
// notFoundDetail, a repository AppError (last owner, concurrent change) is
// kept, and anything else becomes a 500 with internalDetail.
func userWriteError(err error, notFoundDetail, internalDetail string) error {
	if errors.Is(err, apperror.ErrNotFound) {
		return apperror.NewNotFound(notFoundDetail)
	}
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return err
	}
	return apperror.NewInternal(internalDetail)
}
