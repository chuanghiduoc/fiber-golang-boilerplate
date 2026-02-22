package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

type UploadService interface {
	Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*dto.FileResponse, error)
	GetFileInfo(ctx context.Context, id, userID int64) (*dto.FileResponse, error)
	Download(ctx context.Context, id, userID int64) (*sqlc.File, io.ReadCloser, error)
	List(ctx context.Context, userID int64, page, perPage int) ([]dto.FileResponse, int64, error)
	Delete(ctx context.Context, id, userID int64) error
}

type uploadService struct {
	repo    repository.FileRepository
	storage storage.Storage
}

func NewUploadService(repo repository.FileRepository, store storage.Storage) UploadService {
	return &uploadService{repo: repo, storage: store}
}

func (s *uploadService) Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*dto.FileResponse, error) {
	ext := filepath.Ext(filename)
	storagePath := fmt.Sprintf("%d/%s%s", userID, uuid.New().String(), ext)

	if err := s.storage.Put(ctx, storagePath, reader, size, contentType); err != nil {
		return nil, apperror.NewInternal("failed to store file")
	}

	file, err := s.repo.Create(ctx, sqlc.CreateFileParams{
		UserID:       userID,
		OriginalName: filename,
		StoragePath:  storagePath,
		MimeType:     contentType,
		Size:         size,
	})
	if err != nil {
		// Cleanup storage on DB failure
		_ = s.storage.Delete(ctx, storagePath)
		return nil, apperror.NewInternal("failed to save file metadata")
	}

	return s.toFileResponse(file), nil
}

func (s *uploadService) GetFileInfo(ctx context.Context, id, userID int64) (*dto.FileResponse, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to get file")
	}

	if file.UserID != userID {
		return nil, apperror.NewForbidden("you can only access your own files")
	}

	return s.toFileResponse(file), nil
}

func (s *uploadService) Download(ctx context.Context, id, userID int64) (*sqlc.File, io.ReadCloser, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, apperror.NewNotFound("file not found")
		}
		return nil, nil, apperror.NewInternal("failed to get file")
	}

	if file.UserID != userID {
		return nil, nil, apperror.NewForbidden("you can only access your own files")
	}

	reader, err := s.storage.Get(ctx, file.StoragePath)
	if err != nil {
		return nil, nil, apperror.NewInternal("failed to read file from storage")
	}

	return file, reader, nil
}

func (s *uploadService) List(ctx context.Context, userID int64, page, perPage int) ([]dto.FileResponse, int64, error) {
	limit, offset := pagination.LimitOffset(page, perPage)

	// Note: List and Count are separate queries; minor pagination inconsistency is acceptable for read-only operations.
	files, err := s.repo.ListByUserID(ctx, userID, limit, offset)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to list files")
	}

	total, err := s.repo.CountByUserID(ctx, userID)
	if err != nil {
		return nil, 0, apperror.NewInternal("failed to count files")
	}

	responses := make([]dto.FileResponse, len(files))
	for i, f := range files {
		responses[i] = *s.toFileResponse(&f)
	}

	return responses, total, nil
}

func (s *uploadService) Delete(ctx context.Context, id, userID int64) error {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found")
		}
		return apperror.NewInternal("failed to get file")
	}

	if file.UserID != userID {
		return apperror.NewForbidden("you can only delete your own files")
	}

	// Soft delete — do NOT remove from storage so the file can be restored.
	if _, err := s.repo.Delete(ctx, id); err != nil {
		return apperror.NewInternal("failed to delete file metadata")
	}

	slog.Info("file soft-deleted",
		slog.Int64("file_id", id),
		slog.String("path", file.StoragePath),
	)

	return nil
}

func (s *uploadService) toFileResponse(file *sqlc.File) *dto.FileResponse {
	return &dto.FileResponse{
		ID:           file.ID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		URL:          s.storage.URL(file.StoragePath),
		CreatedAt:    file.CreatedAt.Time,
	}
}
