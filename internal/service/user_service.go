package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const (
	bcryptCost         = 12
	maxLoginAttempts   = 5
	lockoutDuration    = 15 * time.Minute
	loginAttemptPrefix = "login_attempts:"
)

// dummyPasswordHash equalizes the bcrypt work for unknown-email logins so the
// response time does not reveal whether an account exists.
var dummyPasswordHash = func() []byte {
	hash, _ := bcrypt.GenerateFromPassword([]byte("timing-only-dummy-password"), bcryptCost)
	return hash
}()

type UserService interface {
	Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error)
	Authenticate(ctx context.Context, req dto.LoginRequest) (*dto.UserResponse, error)
	FindOrCreateByGoogle(ctx context.Context, googleID, email, name string) (*dto.UserResponse, error)
	GetByID(ctx context.Context, id int64) (*dto.UserResponse, error)
	List(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error)
	Update(ctx context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error)
	VerifyPassword(ctx context.Context, userID int64, password string) error
	Delete(ctx context.Context, id int64) error
	ChangePassword(ctx context.Context, userID int64, req dto.ChangePasswordRequest) error
	RevokeSessions(ctx context.Context, userID int64) error
	// SetVerificationProvisioner wires registration email provisioning into the
	// user-creation transaction. When unset, registration falls back to the
	// handler's post-commit verification email.
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
	sessions                 session.Store
	verificationProvisioner  VerificationProvisioner
}

func NewUserService(
	repo repository.UserRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	requireEmailVerification bool,
	appCache cache.Cache,
	txManager *database.TxManager,
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
		sessions:                 sessionStore,
	}
}

func (s *userService) Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error) {
	email := normalizeEmail(req.Email)
	name := strings.TrimSpace(req.Name)

	existing, err := s.repo.GetByEmail(ctx, email)
	if err != nil && !errors.Is(err, apperror.ErrNotFound) {
		return nil, apperror.NewInternal("failed to check existing user")
	}
	if existing != nil {
		return nil, apperror.NewCode(apperror.CodeUserEmailTaken, "email already registered")
	}
	if err := ensureEmailNotBanned(ctx, s.repo, email); err != nil {
		return nil, err
	}

	// Hash before opening a transaction: bcrypt is intentionally expensive and
	// must not hold a pooled database connection for hundreds of milliseconds.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return nil, apperror.NewInternal("failed to hash password")
	}

	var user *sqlc.User
	if s.verificationProvisioner != nil &&
		s.txManager != nil &&
		s.verificationProvisioner.SupportsTransactionalEnqueue() {
		// Create the user, verification token, and email job in one transaction.
		// A provisioning failure rolls the user back: registration fails closed
		// rather than committing an account that cannot receive its link.
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			created, createErr := s.createUser(ctx, repository.NewUserRepository(tx), email, name, hash)
			if createErr != nil {
				return createErr
			}
			if provErr := s.verificationProvisioner.SendVerificationTx(ctx, tx, created.ID, created.Email); provErr != nil {
				slog.Error("failed to provision registration verification email",
					slog.Int64("user_id", created.ID),
					slog.String("email", created.Email),
					slog.Any("error", provErr),
				)
				// Provisioning details (including context deadlines) are for the
				// logs only. Keep the public registration contract at 500.
				return apperror.NewInternal("failed to create user")
			}
			user = created
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		user, err = s.createUser(ctx, s.repo, email, name, hash)
		if err != nil {
			return nil, err
		}
	}

	// A previous failed-login lockout must not survive registration.
	_ = s.cache.Delete(ctx, loginAttemptPrefix+email)

	return ToUserResponse(user), nil
}

// createUser creates a local account with a precomputed password hash through
// repo, which may be backed by the pool or by a caller-owned transaction.
func (s *userService) createUser(
	ctx context.Context,
	repo repository.UserRepository,
	email, name string,
	hash []byte,
) (*sqlc.User, error) {
	user, err := repo.Create(ctx, sqlc.CreateUserParams{
		Email:        email,
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         name,
	})
	if err != nil {
		if repository.IsUniqueViolation(err) {
			return nil, apperror.NewCode(apperror.CodeUserEmailTaken, "email already registered")
		}
		return nil, apperror.NewInternal("failed to create user")
	}
	return user, nil
}

func (s *userService) SetVerificationProvisioner(p VerificationProvisioner) {
	s.verificationProvisioner = p
}

func (s *userService) RegistrationEmailsTransactional() bool {
	return s.verificationProvisioner != nil &&
		s.txManager != nil &&
		s.verificationProvisioner.SupportsTransactionalEnqueue()
}

func (s *userService) Authenticate(ctx context.Context, req dto.LoginRequest) (*dto.UserResponse, error) {
	email := normalizeEmail(req.Email)

	// Look up the account before applying a per-email lockout. Otherwise an
	// attacker can pre-lock an address that does not exist yet, and the real
	// owner is locked out immediately after registering.
	user, err := s.repo.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			// Spend the same bcrypt time as an existing account so login
			// timing cannot be used for account enumeration.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(req.Password))
			// Never create a lockout key for a nonexistent account.
			return nil, apperror.NewCode(apperror.CodeAuthInvalidCredentials, "invalid email or password")
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	// Check lockout (fail-closed: deny login if cache is unavailable)
	cacheKey := loginAttemptPrefix + email
	locked, cacheErr := s.checkLoginLockout(ctx, cacheKey)
	if cacheErr != nil {
		return nil, apperror.NewInternal("unable to verify login attempts, please try again later")
	}
	if locked {
		return nil, apperror.NewTooManyRequests(fmt.Sprintf("account temporarily locked, try again in %d minutes", int(lockoutDuration.Minutes())))
	}

	if !user.PasswordHash.Valid {
		s.incrementLoginAttempts(ctx, cacheKey)
		return nil, apperror.NewCode(apperror.CodeAuthInvalidCredentials, "invalid email or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(req.Password)); err != nil {
		s.incrementLoginAttempts(ctx, cacheKey)
		return nil, apperror.NewCode(apperror.CodeAuthInvalidCredentials, "invalid email or password")
	}

	if s.requireEmailVerification && !user.EmailVerifiedAt.Valid {
		return nil, apperror.NewCode(apperror.CodeAuthEmailNotVerified, "email not verified")
	}

	// Clear attempts on success
	if err := s.cache.Delete(ctx, cacheKey); err != nil {
		slog.Warn("cache error clearing login attempts", slog.String("key", cacheKey), slog.Any("error", err))
	}
	return ToUserResponse(user), nil
}

func (s *userService) incrementLoginAttempts(ctx context.Context, key string) {
	// Atomic increment so parallel failed logins cannot race the counter and
	// slip past the lockout threshold.
	if _, err := s.cache.Increment(ctx, key, lockoutDuration); err != nil {
		slog.Error("cache error storing login attempts — brute force protection degraded", slog.String("key", key), slog.Any("error", err))
	}
}

// checkLoginLockout verifies that the account is not locked out.
// Returns an error if the cache read fails (fail-closed to protect against brute force when cache is down).
func (s *userService) checkLoginLockout(ctx context.Context, cacheKey string) (bool, error) {
	data, cacheErr := s.cache.Get(ctx, cacheKey)
	if cacheErr != nil {
		slog.Error("cache unavailable for login lockout check — denying request for safety", slog.String("key", cacheKey), slog.Any("error", cacheErr))
		return false, cacheErr
	}
	if data != nil {
		attempts, _ := strconv.Atoi(string(data))
		if attempts >= maxLoginAttempts {
			return true, nil
		}
	}
	return false, nil
}

func (s *userService) FindOrCreateByGoogle(ctx context.Context, googleID, email, name string) (*dto.UserResponse, error) {
	user, err := s.findOrCreateByGoogleUser(ctx, googleID, email, name)
	if err != nil {
		return nil, err
	}
	return ToUserResponse(user), nil
}

func (s *userService) findOrCreateByGoogleUser(ctx context.Context, googleID, email, name string) (*sqlc.User, error) {
	email = normalizeEmail(email)

	findOrCreate := func(repo repository.UserRepository) (*sqlc.User, error) {
		user, err := repo.GetByGoogleID(ctx, googleID)
		if err == nil {
			return user, nil
		}
		if !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to find user by google id")
		}

		existing, err := repo.GetByEmail(ctx, email)
		if err == nil {
			// Never silently attach a Google identity to an unverified local
			// account. An attacker could otherwise pre-register a victim's
			// address and take over the account when the victim uses Google.
			if !existing.EmailVerifiedAt.Valid {
				return nil, apperror.NewConflict("an account with this email already exists; verify it before linking Google")
			}
			linked, linkErr := repo.LinkGoogleAccount(ctx, sqlc.LinkGoogleAccountParams{
				GoogleID: pgtype.Text{String: googleID, Valid: true},
				ID:       existing.ID,
			})
			if linkErr != nil {
				if repository.IsUniqueViolation(linkErr) {
					return nil, apperror.NewConflict("this Google account is already linked to another user")
				}
				return nil, apperror.NewInternal("failed to link google account")
			}
			return linked, nil
		}
		if !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to find user by email")
		}
		if err := ensureEmailNotBanned(ctx, repo, email); err != nil {
			return nil, err
		}

		newUser, err := repo.CreateOAuthUser(ctx, sqlc.CreateOAuthUserParams{
			Email:        email,
			Name:         name,
			GoogleID:     pgtype.Text{String: googleID, Valid: true},
			AuthProvider: "google",
		})
		if err != nil {
			return nil, err
		}
		return newUser, nil
	}

	if s.txManager != nil {
		var result *sqlc.User
		txErr := s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			txUserRepo := repository.NewUserRepository(tx)
			var err error
			result, err = findOrCreate(txUserRepo)
			return err
		})
		if txErr != nil {
			if user, retryErr := s.retryOAuthLookup(ctx, googleID, email, txErr); retryErr == nil {
				if user != nil {
					return user, nil
				}
			} else {
				var appErr *apperror.AppError
				if errors.As(retryErr, &appErr) {
					return nil, retryErr
				}
			}
			return nil, apperror.NewInternal("failed to create oauth user")
		}
		return result, nil
	}

	result, err := findOrCreate(s.repo)
	if err != nil {
		if user, retryErr := s.retryOAuthLookup(ctx, googleID, email, err); retryErr == nil {
			if user != nil {
				return user, nil
			}
		} else {
			var appErr *apperror.AppError
			if errors.As(retryErr, &appErr) {
				return nil, retryErr
			}
		}
		return nil, apperror.NewInternal("failed to create oauth user")
	}
	return result, nil
}

// retryOAuthLookup handles the concurrent-create race. It returns a user when
// another request won, or nil to let the caller surface the original error.
func (s *userService) retryOAuthLookup(ctx context.Context, googleID, email string, cause error) (*sqlc.User, error) {
	if !repository.IsUniqueViolation(cause) {
		return nil, cause
	}
	if user, err := s.repo.GetByGoogleID(ctx, googleID); err == nil {
		return user, nil
	}
	if _, err := s.repo.GetByEmail(ctx, email); err == nil {
		return nil, apperror.NewConflict("an account with this email already exists; verify it before linking Google")
	}
	return nil, cause
}

func (s *userService) GetByID(ctx context.Context, id int64) (*dto.UserResponse, error) {
	user, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
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

func (s *userService) Update(ctx context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error) {
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	name := existing.Name
	email := existing.Email

	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Email != nil {
		requested := normalizeEmail(*req.Email)
		if requested != normalizeEmail(existing.Email) {
			dup, err := s.repo.GetByEmail(ctx, requested)
			if err != nil && !errors.Is(err, apperror.ErrNotFound) {
				return nil, apperror.NewInternal("failed to check email availability")
			}
			if dup != nil {
				return nil, apperror.NewConflict("email already in use")
			}
			if err := ensureEmailNotBanned(ctx, s.repo, requested); err != nil {
				return nil, err
			}
			email = requested
		}
	}

	emailChanged := normalizeEmail(existing.Email) != normalizeEmail(email)

	doUpdate := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) (*sqlc.User, error) {
		user, err := userRepo.Update(ctx, sqlc.UpdateUserParams{
			ID:          id,
			Name:        name,
			Email:       email,
			UpdateEmail: req.Email != nil,
		})
		if err != nil {
			if repository.IsUniqueViolation(err) {
				return nil, apperror.NewConflict("email already in use")
			}
			return nil, apperror.NewInternal("failed to update user")
		}
		if emailChanged {
			// Changing the identity address must end every existing session.
			if err := refreshRepo.DeleteByUserID(ctx, id); err != nil {
				return nil, apperror.NewInternal("failed to revoke sessions")
			}
		}
		return user, nil
	}

	var user *sqlc.User
	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			var updateErr error
			user, updateErr = doUpdate(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
			return updateErr
		})
	} else {
		user, err = doUpdate(s.repo, s.refreshTokenRepo)
	}
	if err != nil {
		return nil, err
	}
	if emailChanged && s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}

	return ToUserResponse(user), nil
}

// VerifyPassword checks a plaintext password against the stored hash. It is
// used to require re-authentication for sensitive profile changes.
func ensureEmailNotBanned(ctx context.Context, repo repository.UserRepository, email string) error {
	_, err := repo.GetBannedByEmail(ctx, email)
	switch {
	case err == nil:
		return apperror.NewConflict("email is not available")
	case errors.Is(err, apperror.ErrNotFound):
		return nil
	default:
		return apperror.NewInternal("failed to check email availability")
	}
}

func (s *userService) VerifyPassword(ctx context.Context, userID int64, password string) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to get user")
	}
	if !user.PasswordHash.Valid {
		return apperror.NewBadRequest("current password is incorrect")
	}

	cacheKey := loginAttemptPrefix + normalizeEmail(user.Email)
	locked, cacheErr := s.checkLoginLockout(ctx, cacheKey)
	if cacheErr != nil {
		return apperror.NewInternal("unable to verify login attempts, please try again later")
	}
	if locked {
		return apperror.NewTooManyRequests(fmt.Sprintf("account temporarily locked, try again in %d minutes", int(lockoutDuration.Minutes())))
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(password)) != nil {
		s.incrementLoginAttempts(ctx, cacheKey)
		return apperror.NewBadRequest("current password is incorrect")
	}
	_ = s.cache.Delete(ctx, cacheKey)
	return nil
}

func (s *userService) Delete(ctx context.Context, id int64) error {
	target, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to get user")
	}
	if target.Role == dto.RoleAdmin {
		admins, err := s.repo.CountActiveAdmins(ctx)
		if err != nil {
			return apperror.NewInternal("failed to count admins")
		}
		if admins <= 1 {
			return apperror.NewCode(apperror.CodeUserLastAdmin, "cannot delete the last active admin")
		}
	}

	doDelete := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		_, err := userRepo.Delete(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			var appErr *apperror.AppError
			if errors.As(err, &appErr) {
				return err
			}
			return apperror.NewInternal("failed to delete user")
		}
		// Revoke all refresh tokens so a deleted user's sessions cannot outlive
		// the account. Failing here rolls back the delete when run in a tx.
		if err := refreshRepo.DeleteByUserID(ctx, id); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	}

	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doDelete(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
		})
	} else {
		err = doDelete(s.repo, s.refreshTokenRepo)
	}
	if err != nil {
		return err
	}

	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}
	return nil
}

// RevokeSessions invalidates every access and refresh token for a user, used
// by the "logout all devices" endpoint.
func (s *userService) RevokeSessions(ctx context.Context, userID int64) error {
	doRevoke := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		if _, err := userRepo.RevokeSessions(ctx, userID); err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to revoke sessions")
		}
		if err := refreshRepo.DeleteByUserID(ctx, userID); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	}

	var err error
	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doRevoke(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
		})
	} else {
		err = doRevoke(s.repo, s.refreshTokenRepo)
	}
	if err != nil {
		return err
	}
	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, userID)
	}
	return nil
}

func (s *userService) ChangePassword(ctx context.Context, userID int64, req dto.ChangePasswordRequest) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to get user")
	}

	if !user.PasswordHash.Valid {
		return apperror.NewBadRequest("cannot change password for OAuth accounts")
	}

	if err := s.VerifyPassword(ctx, userID, req.CurrentPassword); err != nil {
		return err
	}
	if req.CurrentPassword == req.NewPassword {
		return apperror.NewBadRequest("new password must be different from the current password")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		return apperror.NewInternal("failed to hash password")
	}

	doChange := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		if _, err := userRepo.UpdatePassword(ctx, sqlc.UpdateUserPasswordParams{
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			ID:           userID,
		}); err != nil {
			return apperror.NewInternal("failed to update password")
		}
		// A password change must invalidate every existing session.
		if err := refreshRepo.DeleteByUserID(ctx, userID); err != nil {
			return apperror.NewInternal("failed to revoke sessions")
		}
		return nil
	}

	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doChange(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
		})
	} else {
		err = doChange(s.repo, s.refreshTokenRepo)
	}
	if err != nil {
		return err
	}

	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, userID)
	}
	_ = s.cache.Delete(ctx, loginAttemptPrefix+normalizeEmail(user.Email))

	return nil
}

func ToUserResponse(user *sqlc.User) *dto.UserResponse {
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
