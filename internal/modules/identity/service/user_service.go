package service

import (
	"context"
	"errors"
	"log/slog"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const bcryptCost = 12

// UserService owns the account lifecycle. Its methods are split by
// responsibility across this package: registration.go, authentication.go and
// lockout.go, oauth_google.go, profile.go (update/delete/sessions), and
// password.go.
type UserService interface {
	Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error)
	Authenticate(ctx context.Context, req dto.LoginRequest) (*dto.UserResponse, error)
	// SignInWithGoogle signs in an existing or linkable account, or creates
	// one when req.AllowSignUp is set (see GoogleSignIn). The caller's
	// Complete hook admits the account and issues the session in the same
	// transaction as the auth.google_login audit row.
	SignInWithGoogle(ctx context.Context, req GoogleSignIn) (*dto.UserResponse, error)
	GetByID(ctx context.Context, id int64) (*dto.UserResponse, error)
	List(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error)
	Update(ctx context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error)
	VerifyPassword(ctx context.Context, userID int64, password string) error
	Delete(ctx context.Context, id int64) error
	ChangePassword(ctx context.Context, userID int64, req dto.ChangePasswordRequest) error
	RevokeSessions(ctx context.Context, userID int64) error
	// SetVerificationProvisioner wires registration email provisioning into the
	// user-creation transaction. When unset, or when the provisioner cannot
	// enqueue transactionally, registration falls back to the handler's
	// post-commit verification email.
	SetVerificationProvisioner(p VerificationProvisioner)
	// RegistrationEmailsTransactional reports whether Register commits the
	// verification email with the user. Callers must not send a second email
	// when it returns true.
	RegistrationEmailsTransactional() bool
}

type userService struct {
	repo                     repository.UserRepository
	refreshTokenRepo         repository.RefreshTokenRepository
	requireEmailVerification bool
	cache                    cache.Cache
	txManager                *database.TxManager
	audit                    audit.TxRecorder
	sessions                 session.Store
	verificationProvisioner  VerificationProvisioner
	webhooks                 webhookcontracts.Dispatcher
}

func NewUserService(
	repo repository.UserRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	requireEmailVerification bool,
	appCache cache.Cache,
	txManager *database.TxManager,
	auditRecorder audit.TxRecorder,
	sessions ...session.Store,
) UserService {
	var sessionStore session.Store
	if len(sessions) > 0 {
		sessionStore = sessions[0]
	}
	return &userService{
		repo:                     repo,
		refreshTokenRepo:         refreshTokenRepo,
		requireEmailVerification: requireEmailVerification,
		cache:                    appCache,
		txManager:                txManager,
		audit:                    auditRecorder,
		sessions:                 sessionStore,
	}
}

// fallbackRepos are the pool-backed repositories inTx uses without a
// transaction manager.
func (s *userService) fallbackRepos() repos {
	return repos{users: s.repo, refresh: s.refreshTokenRepo}
}

func (s *userService) SetVerificationProvisioner(p VerificationProvisioner) {
	s.verificationProvisioner = p
}

func (s *userService) RegistrationEmailsTransactional() bool {
	return s.verificationProvisioner != nil &&
		s.txManager != nil &&
		s.verificationProvisioner.SupportsTransactionalEnqueue()
}

// getUser loads an active account in the request tenant as a public error.
func (s *userService) getUser(ctx context.Context, id int64) (*identitysqlc.User, error) {
	user, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
	}
	return user, nil
}

func (s *userService) GetByID(ctx context.Context, id int64) (*dto.UserResponse, error) {
	user, err := s.getUser(ctx, id)
	if err != nil {
		return nil, err
	}
	return ToUserResponse(user), nil
}

func (s *userService) List(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}

	users, err := s.repo.ListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list users")
	}

	hasMore := len(users) > pageSize
	if hasMore {
		users = users[:pageSize]
	}

	responses := make([]dto.UserResponse, len(users))
	for i := range users {
		responses[i] = *ToUserResponse(&users[i])
	}

	return responses, hasMore, nil
}

// userWriteError maps a failed user-row write: a missing row becomes 404 with
// notFoundDetail, a repository AppError (last admin, concurrent change) is
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

// invalidateSessions drops the cached session state after a committed change
// that bumped the user's token version. The database is already
// authoritative, so a cache failure only delays the effect until the cached
// entry expires; it is logged, not returned.
func invalidateSessions(ctx context.Context, sessions session.Store, userID int64) {
	if sessions == nil {
		return
	}
	if err := sessions.Invalidate(ctx, userID); err != nil {
		slog.Warn("failed to invalidate cached session state",
			slog.Int64("user_id", userID),
			slog.Any("error", err),
		)
	}
}

func ToUserResponse(user *identitysqlc.User) *dto.UserResponse {
	resp := &dto.UserResponse{
		ID:            user.ID,
		Email:         user.Email,
		Name:          user.Name,
		Role:          user.Role,
		EmailVerified: user.EmailVerifiedAt.Valid,
		HasPassword:   user.PasswordHash.Valid,
		CreatedAt:     user.CreatedAt.Time,
		UpdatedAt:     user.UpdatedAt.Time,
		TokenVersion:  user.TokenVersion,
		TenantID:      user.TenantID,
	}
	if user.DeletedAt.Valid {
		deletedAt := user.DeletedAt.Time
		resp.DeletedAt = &deletedAt
	}
	if user.BannedAt.Valid {
		bannedAt := user.BannedAt.Time
		resp.BannedAt = &bannedAt
	}
	return resp
}
