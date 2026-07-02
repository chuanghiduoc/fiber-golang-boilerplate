package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
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
	GetStats(ctx context.Context) (*dto.AdminStatsResponse, error)
}

type adminService struct {
	userRepo         repository.UserRepository
	fileRepo         repository.FileRepository
	refreshTokenRepo repository.RefreshTokenRepository
	storage          storage.Storage
	txManager        *database.TxManager
}

func NewAdminService(
	userRepo repository.UserRepository,
	fileRepo repository.FileRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	store storage.Storage,
	txManager *database.TxManager,
) AdminService {
	return &adminService{
		userRepo: userRepo, fileRepo: fileRepo,
		refreshTokenRepo: refreshTokenRepo, storage: store,
		txManager: txManager,
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
	user, err := s.userRepo.UpdateRole(ctx, sqlc.UpdateUserRoleParams{
		ID:   id,
		Role: role,
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to update user role")
	}

	return ToUserResponse(user), nil
}

func (s *adminService) BanUser(ctx context.Context, id int64) error {
	doBan := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		_, err := userRepo.Delete(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found or already banned")
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
		return s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doBan(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
		})
	}

	return doBan(s.userRepo, s.refreshTokenRepo)
}

func (s *adminService) UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error) {
	user, err := s.userRepo.Restore(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found or not banned")
		}
		return nil, apperror.NewInternal("failed to unban user")
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
		f := files[i]
		responses[i] = dto.FileResponse{
			ID:           f.ID,
			OriginalName: f.OriginalName,
			MimeType:     f.MimeType,
			Size:         f.Size,
			URL:          s.storage.URL(f.StoragePath),
			CreatedAt:    f.CreatedAt.Time,
		}
	}

	return responses, hasMore, nil
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
