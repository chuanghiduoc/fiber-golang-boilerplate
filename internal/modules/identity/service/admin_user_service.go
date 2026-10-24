package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
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
	audit            audit.TxRecorder
	sessions         session.Store
}

func NewAdminUserService(
	userRepo repository.UserRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	txManager *database.TxManager,
	auditRecorder audit.TxRecorder,
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
		audit:            auditRecorder,
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

	previousRole := ""
	update := func(userRepo repository.UserRepository) (*identitysqlc.User, error) {
		existing, err := userRepo.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return nil, apperror.NewNotFound("user not found")
			}
			return nil, apperror.NewInternal("failed to get user")
		}
		previousRole = existing.Role

		// Do not allow the final admin to be demoted; that would lock every
		// administrator out of the system.
		if existing.Role == dto.RoleAdmin && role != dto.RoleAdmin {
			if err := ensureNotLastAdmin(ctx, userRepo, "cannot demote the last active admin"); err != nil {
				return nil, err
			}
		}

		user, err := userRepo.UpdateRole(ctx, identitysqlc.UpdateUserRoleParams{
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
		return user, nil
	}

	var user *identitysqlc.User
	var err error
	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			updated, updateErr := update(repository.NewUserRepository(tx))
			if updateErr != nil {
				return updateErr
			}
			event := audit.NewServiceEvent(ctx, "admin.user.role_changed", "user")
			event.ResourceID = strconv.FormatInt(id, 10)
			event.Metadata = map[string]any{"from_role": previousRole, "to_role": role}
			if auditErr := audit.RecordInTx(ctx, s.audit, tx, event); auditErr != nil {
				return auditWriteError(auditErr)
			}
			user = updated
			return nil
		})
	} else {
		user, err = update(s.userRepo)
		if err == nil {
			event := audit.NewServiceEvent(ctx, "admin.user.role_changed", "user")
			event.ResourceID = strconv.FormatInt(id, 10)
			event.Metadata = map[string]any{"to_role": role}
			if auditErr := audit.RecordInTx(ctx, s.audit, nil, event); auditErr != nil {
				return nil, auditWriteError(auditErr)
			}
		}
	}
	if err != nil {
		return nil, err
	}

	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}
	return ToUserResponse(user), nil
}

func (s *adminUserService) BanUser(ctx context.Context, id int64) error {
	ban := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		existing, err := userRepo.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found or already banned")
			}
			return apperror.NewInternal("failed to get user")
		}
		if existing.Role == dto.RoleAdmin {
			if err := ensureNotLastAdmin(ctx, userRepo, "cannot ban the last active admin"); err != nil {
				return err
			}
		}

		if _, err := userRepo.Ban(ctx, id); err != nil {
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

	var err error
	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			if banErr := ban(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx)); banErr != nil {
				return banErr
			}
			event := audit.NewServiceEvent(ctx, "admin.user.banned", "user")
			event.ResourceID = strconv.FormatInt(id, 10)
			if auditErr := audit.RecordInTx(ctx, s.audit, tx, event); auditErr != nil {
				return auditWriteError(auditErr)
			}
			return nil
		})
	} else {
		err = ban(s.userRepo, s.refreshTokenRepo)
		if err == nil {
			event := audit.NewServiceEvent(ctx, "admin.user.banned", "user")
			event.ResourceID = strconv.FormatInt(id, 10)
			if auditErr := audit.RecordInTx(ctx, s.audit, nil, event); auditErr != nil {
				return auditWriteError(auditErr)
			}
		}
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
	restore := func(userRepo repository.UserRepository) (*identitysqlc.User, error) {
		user, err := userRepo.Restore(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return nil, apperror.NewNotFound("user not found or not banned")
			}
			if database.IsUniqueViolation(err) {
				return nil, apperror.NewConflict("the original email is already in use by another account")
			}
			return nil, apperror.NewInternal("failed to unban user")
		}
		return user, nil
	}

	var user *identitysqlc.User
	var err error
	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			restored, restoreErr := restore(repository.NewUserRepository(tx))
			if restoreErr != nil {
				return restoreErr
			}
			event := audit.NewServiceEvent(ctx, "admin.user.unbanned", "user")
			event.ResourceID = strconv.FormatInt(id, 10)
			if auditErr := audit.RecordInTx(ctx, s.audit, tx, event); auditErr != nil {
				return auditWriteError(auditErr)
			}
			user = restored
			return nil
		})
	} else {
		user, err = restore(s.userRepo)
		if err == nil {
			event := audit.NewServiceEvent(ctx, "admin.user.unbanned", "user")
			event.ResourceID = strconv.FormatInt(id, 10)
			if auditErr := audit.RecordInTx(ctx, s.audit, nil, event); auditErr != nil {
				return nil, auditWriteError(auditErr)
			}
		}
	}
	if err != nil {
		return nil, err
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

// ensureNotLastAdmin returns 409 user.last_admin when removing one admin would
// leave the tenant without any. It first takes the per-tenant admin-change
// lock, so concurrent demote/ban/delete requests run one at a time and the
// count below sees the previous one's committed result (READ COMMITTED takes a
// fresh snapshot per statement). Without that lock the protect_last_admin
// trigger would still keep one admin, but two concurrent requests can
// deadlock on each other's rows instead of getting a clean 409. The trigger
// remains the database-level guarantee for writes outside this service.
func ensureNotLastAdmin(ctx context.Context, userRepo repository.UserRepository, detail string) error {
	if err := userRepo.LockAdminChanges(ctx); err != nil {
		return apperror.NewInternal("failed to lock admin changes")
	}
	admins, err := userRepo.CountActiveAdmins(ctx)
	if err != nil {
		return apperror.NewInternal("failed to count admins")
	}
	if admins <= 1 {
		return apperror.NewCode(apperror.CodeUserLastAdmin, detail)
	}
	return nil
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
