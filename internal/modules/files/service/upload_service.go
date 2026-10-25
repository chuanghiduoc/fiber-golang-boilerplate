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
	"unicode/utf8"

	"github.com/google/uuid"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
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
	Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*File, error)
	GetFileInfo(ctx context.Context, id, userID int64) (*File, error)
	Download(ctx context.Context, id, userID int64) (*File, io.ReadCloser, error)
	List(ctx context.Context, userID int64, limit int, startingAfter string, includeDeleted bool) ([]File, bool, error)
	Delete(ctx context.Context, id, userID int64) error
	Restore(ctx context.Context, id, userID int64) (*File, error)
}

type uploadService struct {
	repo    FileRepository
	tx      Transactor
	storage storage.Storage
}

// NewUploadService builds the user file service. Mutations run through tx, so
// a metadata change and its domain event commit atomically.
func NewUploadService(repo FileRepository, tx Transactor, store storage.Storage) UploadService {
	return &uploadService{repo: repo, tx: tx, storage: store}
}

func (s *uploadService) Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*File, error) {
	tenantID := tenant.EffectiveID(ctx)
	if tenantID <= 0 {
		return nil, apperror.NewInternal("tenant context is required")
	}
	if err := authorizeUserResource(ctx, filescatalog.PermissionFilesWrite, tenantID, userID); err != nil {
		return nil, err
	}

	originalName := cleanOriginalName(filename)
	// Never derive the stored extension from the client-supplied filename: a
	// content-type allowlist must not leave an .html/.svg path that a browser
	// could later render as executable markup.
	//
	// The tenant prefix keeps identically named blobs from different tenants
	// in separate key spaces and makes the owning tenant visible during
	// storage/backup operations.
	storagePath := fmt.Sprintf("%d/%d/%s%s", tenantID, userID, uuid.New().String(), extensionForMIME(contentType))

	if err := s.storage.Put(ctx, storagePath, reader, size, contentType); err != nil {
		return nil, apperror.NewInternal("failed to store file")
	}

	input := NewFile{
		UserID:       userID,
		OriginalName: originalName,
		StoragePath:  storagePath,
		MimeType:     contentType,
		Size:         size,
	}
	var file *File
	// The metadata row and the file.uploaded event commit together; a publish
	// failure rolls the row back and cleans up the blob below.
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		created, createErr := tx.Files.Create(ctx, input)
		if createErr != nil {
			return createErr
		}
		if pubErr := publishEvent(ctx, tx.Events, filescontracts.EventFileUploaded, filescontracts.FileUploadedVersion, tenantID, fileUploaded(created)); pubErr != nil {
			return pubErr
		}
		file = created
		return nil
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
		if errors.Is(err, ErrStoragePathTaken) {
			return nil, apperror.NewConflict("a file with this storage path already exists")
		}
		return nil, apperror.NewInternal("failed to save file metadata")
	}
	return file, nil
}

// cleanOriginalName makes the client filename safe to store and display: valid
// UTF-8 (PostgreSQL rejects anything else with 22021, after the blob was
// already written), no control characters, no path, and at most
// maxOriginalNameRunes runes.
func cleanOriginalName(name string) string {
	name = strings.ToValidUTF8(strings.TrimSpace(filepath.Base(name)), string(utf8.RuneError))
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

func (s *uploadService) GetFileInfo(ctx context.Context, id, userID int64) (*File, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to get file")
	}
	if err := authorizeFile(ctx, filescatalog.PermissionFilesRead, file); err != nil {
		return nil, err
	}

	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
		// Do not reveal that another tenant's file ID exists.
		return nil, apperror.NewNotFound("file not found")
	}

	return file, nil
}

func (s *uploadService) Download(ctx context.Context, id, userID int64) (*File, io.ReadCloser, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, apperror.NewNotFound("file not found")
		}
		return nil, nil, apperror.NewInternal("failed to get file")
	}

	if err := authorizeFile(ctx, filescatalog.PermissionFilesRead, file); err != nil {
		return nil, nil, err
	}
	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
		return nil, nil, apperror.NewNotFound("file not found")
	}

	// The response body is streamed after the handler returns, at which point
	// the request timeout context is canceled. Use a non-canceled context so
	// S3/SeaweedFS object reads do not fail mid-stream; the HTTP server still
	// bounds the response lifetime.
	reader, err := s.storage.Get(context.WithoutCancel(ctx), file.StoragePath)
	if err != nil {
		return nil, nil, storageReadError(file, err)
	}

	return file, reader, nil
}

// storageReadError maps a blob read failure. A missing blob behind live
// metadata is a 404 for the client, but it is data loss for operators, so it
// is logged as an error.
func storageReadError(file *File, err error) *apperror.AppError {
	if errors.Is(err, storage.ErrNotFound) {
		slog.Error("file metadata exists but its blob is missing",
			slog.Int64("file_id", file.ID),
			slog.Int64("tenant_id", file.TenantID),
			slog.String("path", file.StoragePath),
		)
		return apperror.NewNotFound("file not found")
	}
	slog.Error("failed to read file from storage",
		slog.Int64("file_id", file.ID),
		slog.Any("error", err),
	)
	return apperror.NewInternal("failed to read file from storage")
}

func (s *uploadService) List(ctx context.Context, userID int64, limit int, startingAfter string, includeDeleted bool) ([]File, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	if err := authorizeUserResource(ctx, filescatalog.PermissionFilesRead, tenant.EffectiveID(ctx), userID); err != nil {
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
	return files, hasMore, nil
}

func (s *uploadService) Delete(ctx context.Context, id, userID int64) error {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found")
		}
		return apperror.NewInternal("failed to get file")
	}

	if err := authorizeFile(ctx, filescatalog.PermissionFilesDelete, file); err != nil {
		return err
	}
	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
		return apperror.NewNotFound("file not found")
	}

	// Soft delete — do NOT remove from storage so the file can be restored.
	// The soft delete and its file.deleted event commit together.
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		deleted, delErr := tx.Files.Delete(ctx, id)
		if delErr != nil {
			return delErr
		}
		return publishEvent(ctx, tx.Events, filescontracts.EventFileDeleted, filescontracts.FileDeletedVersion, deleted.TenantID, fileDeleted(deleted))
	})
	if err != nil {
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

func (s *uploadService) Restore(ctx context.Context, id, userID int64) (*File, error) {
	file, err := s.repo.GetByIDIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to get file")
	}
	if err := authorizeFile(ctx, filescatalog.PermissionFilesRestore, file); err != nil {
		return nil, err
	}
	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
		return nil, apperror.NewNotFound("file not found")
	}
	if file.DeletedAt == nil {
		return nil, apperror.NewCode(filescatalog.CodeFileNotDeleted, "file is not deleted")
	}

	var restored *File
	// The restore and its file.restored event commit together.
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		row, restoreErr := tx.Files.Restore(ctx, id)
		if restoreErr != nil {
			return restoreErr
		}
		if pubErr := publishEvent(ctx, tx.Events, filescontracts.EventFileRestored, filescontracts.FileRestoredVersion, row.TenantID, fileRestored(row)); pubErr != nil {
			return pubErr
		}
		restored = row
		return nil
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to restore file")
	}
	return restored, nil
}

// buildCursor normalizes the page size and decodes an optional cursor.
func buildCursor(limit int, startingAfter string) (cur pagination.DBCursor, pageSize int, err error) {
	pageSize = pagination.NormalizeLimit(limit)
	cur = pagination.DBCursor{Limit: pagination.RowLimit(pageSize + 1)}
	if startingAfter != "" {
		ts, id, decErr := pagination.DecodeCursor(startingAfter)
		if decErr != nil {
			return pagination.DBCursor{}, 0, apperror.NewBadRequest("invalid cursor")
		}
		cur.HasCursor = true
		cur.CreatedAt = ts
		cur.ID = id
	}
	return cur, pageSize, nil
}
