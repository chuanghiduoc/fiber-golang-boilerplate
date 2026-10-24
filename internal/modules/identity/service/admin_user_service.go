package service

import (
	"context"
	"errors"
	"log/slog"

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

func (s *adminUserService) fallbackRepos() repos {
	return repos{users: s.userRepo, refresh: s.refreshTokenRepo}
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

	var user *identitysqlc.User
	err := inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		updated, previousRole, updateErr := changeRole(ctx, r.users, id, role)
		if updateErr != nil {
			return updateErr
		}
		user = updated
		event := userEvent(ctx, "admin.user.role_changed", id, map[string]any{"from_role": previousRole, "to_role": role})
		return checkAudit(audit.RecordInTx(ctx, s.audit, tx, event))
	})
	if err != nil {
		return nil, err
	}

	invalidateSessions(ctx, s.sessions, id)
	return ToUserResponse(user), nil
}

// changeRole sets the user's role and returns the previous one. The final
// admin is never demoted; that would lock every administrator out.
func changeRole(ctx context.Context, users repository.UserRepository, id int64, role string) (*identitysqlc.User, string, error) {
	existing, err := getUserForAdmin(ctx, users, id, "user not found")
	if err != nil {
		return nil, "", err
	}
	if existing.Role == dto.RoleAdmin && role != dto.RoleAdmin {
		if err := ensureNotLastAdmin(ctx, users, "cannot demote the last active admin"); err != nil {
			return nil, "", err
		}
	}
	user, err := users.UpdateRole(ctx, identitysqlc.UpdateUserRoleParams{ID: id, Role: role})
	if err != nil {
		return nil, "", userWriteError(err, "user not found", "failed to update user role")
	}
	return user, existing.Role, nil
}

func (s *adminUserService) BanUser(ctx context.Context, id int64) error {
	err := inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		if banErr := banAccount(ctx, r, id); banErr != nil {
			return banErr
		}
		return checkAudit(audit.RecordInTx(ctx, s.audit, tx, userEvent(ctx, "admin.user.banned", id, nil)))
	})
	if err != nil {
		return err
	}
	invalidateSessions(ctx, s.sessions, id)
	return nil
}

// banAccount bans the user (which also revokes its API keys through
// credentials_revoked_at) and deletes its refresh tokens. The last active
// admin can never be banned.
func banAccount(ctx context.Context, r repos, id int64) error {
	const notFound = "user not found or already banned"
	existing, err := getUserForAdmin(ctx, r.users, id, notFound)
	if err != nil {
		return err
	}
	if existing.Role == dto.RoleAdmin {
		if err := ensureNotLastAdmin(ctx, r.users, "cannot ban the last active admin"); err != nil {
			return err
		}
	}
	if _, err := r.users.Ban(ctx, id); err != nil {
		return userWriteError(err, notFound, "failed to ban user")
	}
	if err := r.refresh.DeleteByUserID(ctx, id); err != nil {
		return apperror.NewInternal("failed to revoke refresh tokens")
	}
	return nil
}

func (s *adminUserService) UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error) {
	var user *identitysqlc.User
	err := inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		restored, restoreErr := restoreAccount(ctx, r.users, id)
		if restoreErr != nil {
			return restoreErr
		}
		user = restored
		return checkAudit(audit.RecordInTx(ctx, s.audit, tx, userEvent(ctx, "admin.user.unbanned", id, nil)))
	})
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
	invalidateSessions(ctx, s.sessions, id)
	return ToUserResponse(user), nil
}

func restoreAccount(ctx context.Context, users repository.UserRepository, id int64) (*identitysqlc.User, error) {
	user, err := users.Restore(ctx, id)
	switch {
	case err == nil:
		return user, nil
	case errors.Is(err, apperror.ErrNotFound):
		return nil, apperror.NewNotFound("user not found or not banned")
	case database.IsUniqueViolation(err):
		return nil, apperror.NewConflict("the original email is already in use by another account")
	default:
		return nil, apperror.NewInternal("failed to unban user")
	}
}

// getUserForAdmin loads an active account on users (usually the admin
// mutation's transaction) and reports a missing one with notFoundDetail.
func getUserForAdmin(ctx context.Context, users repository.UserRepository, id int64, notFoundDetail string) (*identitysqlc.User, error) {
	user, err := users.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound(notFoundDetail)
		}
		return nil, apperror.NewInternal("failed to get user")
	}
	return user, nil
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
