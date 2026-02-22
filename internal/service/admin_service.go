package service

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type AdminService interface {
	ListUsers(ctx context.Context, page, perPage int) ([]dto.UserResponse, int64, error)
	UpdateRole(ctx context.Context, id int64, role string) (*dto.UserResponse, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error)
	ListFiles(ctx context.Context, page, perPage int) ([]dto.FileResponse, int64, error)
	GetStats(ctx context.Context) (*dto.AdminStatsResponse, error)
}

type adminService struct {
	userRepo         repository.UserRepository
	fileRepo         repository.FileRepository
	refreshTokenRepo repository.RefreshTokenRepository
	storage          storage.Storage
}

func NewAdminService(
	userRepo repository.UserRepository,
	fileRepo repository.FileRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	store storage.Storage,
) AdminService {
	return &adminService{
		userRepo: userRepo, fileRepo: fileRepo,
		refreshTokenRepo: refreshTokenRepo, storage: store,
	}
}

func (s *adminService) ListUsers(ctx context.Context, page, perPage int) ([]dto.UserResponse, int64, error) {
	limit, offset := pagination.LimitOffset(page, perPage)

	// Note: List and Count are separate queries; minor pagination inconsistency is acceptable for read-only operations.
	users, err := s.userRepo.AdminList(ctx, limit, offset)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to list users")
	}

	total, err := s.userRepo.AdminCount(ctx)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to count users")
	}

	responses := make([]dto.UserResponse, len(users))
	for i, u := range users {
		responses[i] = *ToUserResponse(&u)
	}

	return responses, total, nil
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
	_, err := s.userRepo.Delete(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found or already banned")
		}
		return apperror.NewInternal("failed to ban user")
	}

	// Revoke all refresh tokens for banned user
	_ = s.refreshTokenRepo.DeleteByUserID(ctx, id)
	return nil
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

func (s *adminService) ListFiles(ctx context.Context, page, perPage int) ([]dto.FileResponse, int64, error) {
	limit, offset := pagination.LimitOffset(page, perPage)

	// Note: List and Count are separate queries; minor pagination inconsistency is acceptable for read-only operations.
	files, err := s.fileRepo.AdminList(ctx, limit, offset)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to list files")
	}

	total, err := s.fileRepo.AdminCount(ctx)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to count files")
	}

	responses := make([]dto.FileResponse, len(files))
	for i, f := range files {
		responses[i] = dto.FileResponse{
			ID:           f.ID,
			OriginalName: f.OriginalName,
			MimeType:     f.MimeType,
			Size:         f.Size,
			URL:          s.storage.URL(f.StoragePath),
			CreatedAt:    f.CreatedAt.Time,
		}
	}

	return responses, total, nil
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
