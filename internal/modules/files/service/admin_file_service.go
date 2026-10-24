package service

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// AdminFileService owns the files side of the back-office surface: listing all
// metadata (including soft-deleted), admin downloads, takedowns, restores, and
// the files half of the dashboard statistics.
type AdminFileService interface {
	ListFiles(ctx context.Context, limit int, startingAfter string) ([]dto.FileResponse, bool, error)
	Download(ctx context.Context, id int64) (*dto.FileResponse, io.ReadCloser, error)
	DeleteFile(ctx context.Context, id int64) error
	RestoreFile(ctx context.Context, id int64) (*dto.FileResponse, error)
	GetFileStats(ctx context.Context) (FileStats, error)
}

// FileStats is the files-owned aggregation consumed by the admin module.
type FileStats struct {
	TotalFiles    int64
	TotalFileSize int64
}

type adminFileService struct {
	repo    repository.FileRepository
	storage storage.Storage
}

func NewAdminFileService(repo repository.FileRepository, store storage.Storage) AdminFileService {
	return &adminFileService{repo: repo, storage: store}
}

func (s *adminFileService) ListFiles(ctx context.Context, limit int, startingAfter string) ([]dto.FileResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}

	files, err := s.repo.AdminListCursor(ctx, cur)
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

func (s *adminFileService) toAdminFileResponse(file *filesqlc.File) *dto.FileResponse {
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

func (s *adminFileService) Download(ctx context.Context, id int64) (*dto.FileResponse, io.ReadCloser, error) {
	file, err := s.repo.GetByID(ctx, id)
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

func (s *adminFileService) DeleteFile(ctx context.Context, id int64) error {
	if _, err := s.repo.Delete(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found or already deleted")
		}
		return apperror.NewInternal("failed to delete file")
	}
	return nil
}

func (s *adminFileService) RestoreFile(ctx context.Context, id int64) (*dto.FileResponse, error) {
	existing, err := s.repo.GetByIDIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to get file")
	}
	if !existing.DeletedAt.Valid {
		return nil, apperror.NewCode(apperror.CodeFileNotDeleted, "file is not deleted")
	}
	file, err := s.repo.Restore(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found or not deleted")
		}
		return nil, apperror.NewInternal("failed to restore file")
	}
	return s.toAdminFileResponse(file), nil
}

func (s *adminFileService) GetFileStats(ctx context.Context) (FileStats, error) {
	stats, err := s.repo.AdminFileStats(ctx)
	if err != nil {
		return FileStats{}, apperror.NewInternal("failed to get file stats")
	}
	return FileStats{
		TotalFiles:    stats.TotalFiles,
		TotalFileSize: stats.TotalFileSize,
	}, nil
}
