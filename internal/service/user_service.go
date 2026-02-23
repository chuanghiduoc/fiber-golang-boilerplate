package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

const (
	bcryptCost         = 12
	maxLoginAttempts   = 5
	lockoutDuration    = 15 * time.Minute
	loginAttemptPrefix = "login_attempts:"
)

type UserService interface {
	Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error)
	Authenticate(ctx context.Context, req dto.LoginRequest) (*sqlc.User, error)
	FindOrCreateByGoogle(ctx context.Context, googleID, email, name string) (*sqlc.User, error)
	GetByID(ctx context.Context, id int64) (*dto.UserResponse, error)
	List(ctx context.Context, page, perPage int) ([]dto.UserResponse, int64, error)
	Update(ctx context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error)
	Delete(ctx context.Context, id int64) error
	ChangePassword(ctx context.Context, userID int64, req dto.ChangePasswordRequest) error
}

type userService struct {
	repo                     repository.UserRepository
	refreshTokenRepo         repository.RefreshTokenRepository
	requireEmailVerification bool
	cache                    cache.Cache
	txManager                *database.TxManager
}

func NewUserService(
	repo repository.UserRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	requireEmailVerification bool,
	appCache cache.Cache,
	txManager *database.TxManager,
) UserService {
	return &userService{
		repo:                     repo,
		refreshTokenRepo:         refreshTokenRepo,
		requireEmailVerification: requireEmailVerification,
		cache:                    appCache,
		txManager:                txManager,
	}
}

func (s *userService) Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error) {
	existing, err := s.repo.GetByEmail(ctx, req.Email)
	if err != nil && !errors.Is(err, apperror.ErrNotFound) {
		return nil, apperror.NewInternal("failed to check existing user")
	}
	if existing != nil {
		return nil, apperror.NewBadRequest("email already registered")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return nil, apperror.NewInternal("failed to hash password")
	}

	user, err := s.repo.Create(ctx, sqlc.CreateUserParams{
		Email:        req.Email,
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         req.Name,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to create user")
	}

	return ToUserResponse(user), nil
}

func (s *userService) Authenticate(ctx context.Context, req dto.LoginRequest) (*sqlc.User, error) {
	// Check lockout
	cacheKey := loginAttemptPrefix + req.Email
	if data, _ := s.cache.Get(ctx, cacheKey); data != nil {
		attempts, _ := strconv.Atoi(string(data))
		if attempts >= maxLoginAttempts {
			return nil, apperror.NewBadRequest(fmt.Sprintf("account temporarily locked, try again in %d minutes", int(lockoutDuration.Minutes())))
		}
	}

	user, err := s.repo.GetByEmail(ctx, req.Email)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			s.incrementLoginAttempts(ctx, cacheKey)
			return nil, apperror.NewUnauthorized("invalid email or password")
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	if !user.PasswordHash.Valid {
		s.incrementLoginAttempts(ctx, cacheKey)
		return nil, apperror.NewUnauthorized("invalid email or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(req.Password)); err != nil {
		s.incrementLoginAttempts(ctx, cacheKey)
		return nil, apperror.NewUnauthorized("invalid email or password")
	}

	if s.requireEmailVerification && !user.EmailVerifiedAt.Valid {
		return nil, apperror.NewForbidden("email not verified")
	}

	// Clear attempts on success
	_ = s.cache.Delete(ctx, cacheKey)
	return user, nil
}

func (s *userService) incrementLoginAttempts(ctx context.Context, key string) {
	attempts := 1
	if data, _ := s.cache.Get(ctx, key); data != nil {
		attempts, _ = strconv.Atoi(string(data))
		attempts++
	}
	_ = s.cache.Set(ctx, key, []byte(strconv.Itoa(attempts)), lockoutDuration)
}

func (s *userService) FindOrCreateByGoogle(ctx context.Context, googleID, email, name string) (*sqlc.User, error) {
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
			linked, linkErr := repo.LinkGoogleAccount(ctx, sqlc.LinkGoogleAccountParams{
				GoogleID: pgtype.Text{String: googleID, Valid: true},
				ID:       existing.ID,
			})
			if linkErr != nil {
				return nil, apperror.NewInternal("failed to link google account")
			}
			return linked, nil
		}
		if !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to find user by email")
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
			if repository.IsUniqueViolation(txErr) {
				if user, err := s.repo.GetByGoogleID(ctx, googleID); err == nil {
					return user, nil
				}
			}
			return nil, apperror.NewInternal("failed to create oauth user")
		}
		return result, nil
	}

	result, err := findOrCreate(s.repo)
	if err != nil {
		if repository.IsUniqueViolation(err) {
			if user, retryErr := s.repo.GetByGoogleID(ctx, googleID); retryErr == nil {
				return user, nil
			}
		}
		return nil, apperror.NewInternal("failed to create oauth user")
	}
	return result, nil
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

func (s *userService) List(ctx context.Context, page, perPage int) ([]dto.UserResponse, int64, error) {
	limit, offset := pagination.LimitOffset(page, perPage)

	// Note: List and Count are separate queries; minor pagination inconsistency is acceptable for read-only operations.
	users, err := s.repo.List(ctx, limit, offset)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to list users")
	}

	total, err := s.repo.Count(ctx)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to count users")
	}

	responses := make([]dto.UserResponse, len(users))
	for i, u := range users {
		responses[i] = *ToUserResponse(&u)
	}

	return responses, total, nil
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
		name = *req.Name
	}
	if req.Email != nil && *req.Email != existing.Email {
		dup, err := s.repo.GetByEmail(ctx, *req.Email)
		if err != nil && !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to check email availability")
		}
		if dup != nil {
			return nil, apperror.NewBadRequest("email already in use")
		}
		email = *req.Email
	}

	user, err := s.repo.Update(ctx, sqlc.UpdateUserParams{
		ID:    id,
		Name:  name,
		Email: email,
	})
	if err != nil {
		return nil, apperror.NewInternal("failed to update user")
	}

	return ToUserResponse(user), nil
}

func (s *userService) Delete(ctx context.Context, id int64) error {
	doDelete := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		_, err := userRepo.Delete(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to delete user")
		}
		// Revoke all refresh tokens
		_ = refreshRepo.DeleteByUserID(ctx, id)
		return nil
	}

	if s.txManager != nil {
		return s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doDelete(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
		})
	}

	return doDelete(s.repo, s.refreshTokenRepo)
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

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(req.CurrentPassword)); err != nil {
		return apperror.NewBadRequest("current password is incorrect")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		return apperror.NewInternal("failed to hash password")
	}

	_, err = s.repo.UpdatePassword(ctx, sqlc.UpdateUserPasswordParams{
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		ID:           userID,
	})
	if err != nil {
		return apperror.NewInternal("failed to update password")
	}

	return nil
}

func ToUserResponse(user *sqlc.User) *dto.UserResponse {
	return &dto.UserResponse{
		ID:            user.ID,
		Email:         user.Email,
		Name:          user.Name,
		Role:          user.Role,
		EmailVerified: user.EmailVerifiedAt.Valid,
		CreatedAt:     user.CreatedAt.Time,
		UpdatedAt:     user.UpdatedAt.Time,
	}
}
