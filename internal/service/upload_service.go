package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const maxOriginalNameRunes = 255

var mimeExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

type UploadService interface {
	Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*dto.FileResponse, error)
	GetFileInfo(ctx context.Context, id, userID int64) (*dto.FileResponse, error)
	Download(ctx context.Context, id, userID int64) (*dto.FileResponse, io.ReadCloser, error)
	List(ctx context.Context, userID int64, limit int, startingAfter string, includeDeleted bool) ([]dto.FileResponse, bool, error)
	Delete(ctx context.Context, id, userID int64) error
	Restore(ctx context.Context, id, userID int64) (*dto.FileResponse, error)
}

type uploadService struct {
	repo    repository.FileRepository
	storage storage.Storage
}

func NewUploadService(repo repository.FileRepository, store storage.Storage) UploadService {
	return &uploadService{repo: repo, storage: store}
}

func (s *uploadService) Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*dto.FileResponse, error) {
	originalName := cleanOriginalName(filename)
	// Never derive the stored extension from the client-supplied filename: a
	// content-type allowlist must not leave an .html/.svg path that a browser
	// could later render as executable markup.
	storagePath := fmt.Sprintf("%d/%s%s", userID, uuid.New().String(), extensionForMIME(contentType))

	if err := s.storage.Put(ctx, storagePath, reader, size, contentType); err != nil {
		return nil, apperror.NewInternal("failed to store file")
	}

	file, err := s.repo.Create(ctx, sqlc.CreateFileParams{
		UserID:       userID,
		OriginalName: originalName,
		StoragePath:  storagePath,
		MimeType:     contentType,
		Size:         size,
	})
	if err != nil {
		// Cleanup storage on DB failure. The request context may already be
		// canceled; storage cleanup must still run.
		if delErr := s.storage.Delete(context.WithoutCancel(ctx), storagePath); delErr != nil {
			slog.Error("failed to cleanup storage after DB failure",
				slog.String("path", storagePath),
				slog.Any("error", delErr),
			)
		}
		if repository.IsUniqueViolation(err) {
			return nil, apperror.NewConflict("a file with this storage path already exists")
		}
		return nil, apperror.NewInternal("failed to save file metadata")
	}

	return s.toFileResponse(file), nil
}

func cleanOriginalName(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return "upload"
	}
	runes := []rune(name)
	if len(runes) > maxOriginalNameRunes {
		name = string(runes[:maxOriginalNameRunes])
	}
	return name
}

func extensionForMIME(contentType string) string {
	if ext, ok := mimeExtensions[contentType]; ok {
		return ext
	}
	if exts, err := mime.ExtensionsByType(contentType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
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
		// Do not reveal that another tenant's file ID exists.
		return nil, apperror.NewNotFound("file not found")
	}

	return s.toFileResponse(file), nil
}

func (s *uploadService) Download(ctx context.Context, id, userID int64) (*dto.FileResponse, io.ReadCloser, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, apperror.NewNotFound("file not found")
		}
		return nil, nil, apperror.NewInternal("failed to get file")
	}

	if file.UserID != userID {
		return nil, nil, apperror.NewNotFound("file not found")
	}

	// The response body is streamed after the handler returns, at which point
	// the request timeout context is canceled. Use a non-canceled context so
	// S3/MinIO object reads do not fail mid-stream; the HTTP server still
	// bounds the response lifetime.
	reader, err := s.storage.Get(context.WithoutCancel(ctx), file.StoragePath)
	if err != nil {
		return nil, nil, apperror.NewInternal("failed to read file from storage")
	}

	return s.toFileResponse(file), reader, nil
}

func (s *uploadService) List(ctx context.Context, userID int64, limit int, startingAfter string, includeDeleted bool) ([]dto.FileResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}

	files, err := s.repo.ListByUserCursor(ctx, userID, cur, includeDeleted)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list files")
	}

	hasMore := len(files) > pageSize
	if hasMore {
		files = files[:pageSize]
	}

	responses := make([]dto.FileResponse, len(files))
	for i := range files {
		responses[i] = *s.toFileResponse(&files[i])
	}

	return responses, hasMore, nil
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
		return apperror.NewNotFound("file not found")
	}

	// Soft delete — do NOT remove from storage so the file can be restored.
	if _, err := s.repo.Delete(ctx, id); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found")
		}
		return apperror.NewInternal("failed to delete file metadata")
	}

	slog.Info("file soft-deleted",
		slog.Int64("file_id", id),
		slog.String("path", file.StoragePath),
	)

	return nil
}

func (s *uploadService) Restore(ctx context.Context, id, userID int64) (*dto.FileResponse, error) {
	file, err := s.repo.GetByIDIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to get file")
	}
	if file.UserID != userID {
		return nil, apperror.NewNotFound("file not found")
	}
	if !file.DeletedAt.Valid {
		return nil, apperror.NewCode(apperror.CodeFileNotDeleted, "file is not deleted")
	}

	restored, err := s.repo.Restore(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to restore file")
	}
	return s.toFileResponse(restored), nil
}

func (s *uploadService) toFileResponse(file *sqlc.File) *dto.FileResponse {
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
		resp.URL = fmt.Sprintf("/api/v1/files/%d/download", file.ID)
	}
	return resp
}
