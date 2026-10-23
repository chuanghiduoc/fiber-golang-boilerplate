package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type AdminService interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error)
	UpdateRole(ctx context.Context, id int64, role string) (*dto.UserResponse, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error)
	ListFiles(ctx context.Context, limit int, startingAfter string) ([]dto.FileResponse, bool, error)
	Download(ctx context.Context, id int64) (*dto.FileResponse, io.ReadCloser, error)
	DeleteFile(ctx context.Context, id int64) error
	RestoreFile(ctx context.Context, id int64) (*dto.FileResponse, error)
	GetStats(ctx context.Context) (*dto.AdminStatsResponse, error)
}

type adminService struct {
	userRepo         repository.UserRepository
	fileRepo         repository.FileRepository
	refreshTokenRepo repository.RefreshTokenRepository
	storage          storage.Storage
	txManager        *database.TxManager
	sessions         session.Store
}

func NewAdminService(
	userRepo repository.UserRepository,
	fileRepo repository.FileRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	store storage.Storage,
	txManager *database.TxManager,
	sessions ...session.Store,
) AdminService {
	var sessionStore session.Store
	if len(sessions) > 0 {
		sessionStore = sessions[0]
	}
	return &adminService{
		userRepo: userRepo, fileRepo: fileRepo,
		refreshTokenRepo: refreshTokenRepo, storage: store,
		txManager: txManager, sessions: sessionStore,
	}
}

func (s *adminService) ListUsers(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error) {
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

func (s *adminService) UpdateRole(ctx context.Context, id int64, role string) (*dto.UserResponse, error) {
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

func (s *adminService) BanUser(ctx context.Context, id int64) error {
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
		// Revoke all refresh tokens for banned user
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

func (s *adminService) UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error) {
	user, err := s.userRepo.Restore(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found or not banned")
		}
		if repository.IsUniqueViolation(err) {
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

func (s *adminService) ListFiles(ctx context.Context, limit int, startingAfter string) ([]dto.FileResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}

	files, err := s.fileRepo.AdminListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list files")
	}

	hasMore := len(files) > pageSize
	if hasMore {
		files = files[:pageSize]
	}

	responses := make([]dto.FileResponse, len(files))
	for i := range files {
		responses[i] = *s.toAdminFileResponse(&files[i])
	}

	return responses, hasMore, nil
}

func (s *adminService) toAdminFileResponse(file *sqlc.File) *dto.FileResponse {
	resp := &dto.FileResponse{
		ID:           file.ID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt.Time,
	}
	if file.DeletedAt.Valid {
		deletedAt := file.DeletedAt.Time
		resp.DeletedAt = &deletedAt
	} else {
		resp.URL = fmt.Sprintf("/api/v1/admin/files/%d/download", file.ID)
	}
	return resp
}

func (s *adminService) Download(ctx context.Context, id int64) (*dto.FileResponse, io.ReadCloser, error) {
	file, err := s.fileRepo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, apperror.NewNotFound("file not found")
		}
		return nil, nil, apperror.NewInternal("failed to get file")
	}

	reader, err := s.storage.Get(context.WithoutCancel(ctx), file.StoragePath)
	if err != nil {
		return nil, nil, apperror.NewInternal("failed to read file from storage")
	}
	return s.toAdminFileResponse(file), reader, nil
}

func (s *adminService) DeleteFile(ctx context.Context, id int64) error {
	if _, err := s.fileRepo.Delete(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found or already deleted")
		}
		return apperror.NewInternal("failed to delete file")
	}
	return nil
}

func (s *adminService) RestoreFile(ctx context.Context, id int64) (*dto.FileResponse, error) {
	existing, err := s.fileRepo.GetByIDIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to get file")
	}
	if !existing.DeletedAt.Valid {
		return nil, apperror.NewCode(apperror.CodeFileNotDeleted, "file is not deleted")
	}
	file, err := s.fileRepo.Restore(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found or not deleted")
		}
		return nil, apperror.NewInternal("failed to restore file")
	}
	return s.toAdminFileResponse(file), nil
}

func (s *adminService) GetStats(ctx context.Context) (*dto.AdminStatsResponse, error) {
	stats, err := s.userRepo.GetSystemStats(ctx)
	if err != nil {
		return nil, apperror.NewInternal("failed to get system stats")
	}

	return &dto.AdminStatsResponse{
		ActiveUsers:   stats.ActiveUsers,
		DeletedUsers:  stats.DeletedUsers,
		TotalFiles:    stats.TotalFiles,
		TotalFileSize: stats.TotalFileSize,
	}, nil
}
