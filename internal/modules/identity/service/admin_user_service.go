package service

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// AdminUserService owns the identity side of the back-office user surface:
// listing accounts (including soft-deleted), role changes, bans, unbans, and
// the identity half of the dashboard statistics.
type AdminUserService interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error)
	UpdateRole(ctx context.Context, id int64, role string) (*dto.UserResponse, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error)
	GetUserStats(ctx context.Context) (UserStats, error)
}

// UserStats is the identity-owned aggregation consumed by the admin module.
type UserStats struct {
	ActiveUsers  int64
	DeletedUsers int64
}

type adminUserService struct {
	userRepo         repository.UserRepository
	refreshTokenRepo repository.RefreshTokenRepository
	txManager        *database.TxManager
	sessions         session.Store
}

func NewAdminUserService(
	userRepo repository.UserRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	txManager *database.TxManager,
	sessions ...session.Store,
) AdminUserService {
	var sessionStore session.Store
	if len(sessions) > 0 {
		sessionStore = sessions[0]
	}
	return &adminUserService{
		userRepo:         userRepo,
		refreshTokenRepo: refreshTokenRepo,
		txManager:        txManager,
		sessions:         sessionStore,
	}
}

func (s *adminUserService) ListUsers(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}

	users, err := s.userRepo.AdminListCursor(ctx, cur)
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

func (s *adminUserService) UpdateRole(ctx context.Context, id int64, role string) (*dto.UserResponse, error) {
	if role != dto.RoleUser && role != dto.RoleAdmin {
		return nil, apperror.NewBadRequest("role must be one of: user, admin")
	}

	existing, err := s.userRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	// Do not allow the final admin to be demoted; that would lock every
	// administrator out of the system.
	if existing.Role == dto.RoleAdmin && role != dto.RoleAdmin {
		admins, err := s.userRepo.CountActiveAdmins(ctx)
		if err != nil {
			return nil, apperror.NewInternal("failed to count admins")
		}
		if admins <= 1 {
			return nil, apperror.NewCode(apperror.CodeUserLastAdmin, "cannot demote the last active admin")
		}
	}

	user, err := s.userRepo.UpdateRole(ctx, sqlc.UpdateUserRoleParams{
		ID:   id,
		Role: role,
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		var appErr *apperror.AppError
		if errors.As(err, &appErr) {
			return nil, err
		}
		return nil, apperror.NewInternal("failed to update user role")
	}

	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}
	return ToUserResponse(user), nil
}

func (s *adminUserService) BanUser(ctx context.Context, id int64) error {
	existing, err := s.userRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found or already banned")
		}
		return apperror.NewInternal("failed to get user")
	}
	if existing.Role == dto.RoleAdmin {
		admins, err := s.userRepo.CountActiveAdmins(ctx)
		if err != nil {
			return apperror.NewInternal("failed to count admins")
		}
		if admins <= 1 {
			return apperror.NewCode(apperror.CodeUserLastAdmin, "cannot ban the last active admin")
		}
	}

	doBan := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		_, err := userRepo.Ban(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found or already banned")
			}
			var appErr *apperror.AppError
			if errors.As(err, &appErr) {
				return err
			}
			return apperror.NewInternal("failed to ban user")
		}
		// Revoke all refresh tokens for banned user.
		if err := refreshRepo.DeleteByUserID(ctx, id); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	}

	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doBan(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
		})
	} else {
		err = doBan(s.userRepo, s.refreshTokenRepo)
	}
	if err != nil {
		return err
	}
	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}
	return nil
}

func (s *adminUserService) UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error) {
	user, err := s.userRepo.Restore(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found or not banned")
		}
		if database.IsUniqueViolation(err) {
			return nil, apperror.NewConflict("the original email is already in use by another account")
		}
		return nil, apperror.NewInternal("failed to unban user")
	}

	if err := s.refreshTokenRepo.DeleteByUserID(ctx, id); err != nil {
		// Restore already committed; stale refresh tokens carry an older token
		// version and are rejected by the refresh handler, but log the cleanup
		// failure for operators.
		slog.Warn("failed to clear refresh tokens after unban",
			slog.Int64("user_id", id),
			slog.Any("error", err),
		)
	}
	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}
	return ToUserResponse(user), nil
}

func (s *adminUserService) GetUserStats(ctx context.Context) (UserStats, error) {
	stats, err := s.userRepo.AdminUserStats(ctx)
	if err != nil {
		return UserStats{}, apperror.NewInternal("failed to get user stats")
	}
	return UserStats{
		ActiveUsers:  stats.ActiveUsers,
		DeletedUsers: stats.DeletedUsers,
	}, nil
}
