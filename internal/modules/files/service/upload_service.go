package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
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
	Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*dto.FileResponse, error)
	GetFileInfo(ctx context.Context, id, userID int64) (*dto.FileResponse, error)
	Download(ctx context.Context, id, userID int64) (*dto.FileResponse, io.ReadCloser, error)
	List(ctx context.Context, userID int64, limit int, startingAfter string, includeDeleted bool) ([]dto.FileResponse, bool, error)
	Delete(ctx context.Context, id, userID int64) error
	Restore(ctx context.Context, id, userID int64) (*dto.FileResponse, error)
}

type uploadService struct {
	repo      filesrepo.FileRepository
	storage   storage.Storage
	webhooks  webhookcontracts.Dispatcher
	txManager *database.TxManager
}

func NewUploadService(repo filesrepo.FileRepository, store storage.Storage) UploadService {
	return &uploadService{repo: repo, storage: store}
}

func (s *uploadService) Upload(ctx context.Context, userID int64, filename string, reader io.Reader, size int64, contentType string) (*dto.FileResponse, error) {
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

	params := filesqlc.CreateFileParams{
		TenantID:     tenantID,
		UserID:       userID,
		OriginalName: originalName,
		StoragePath:  storagePath,
		MimeType:     contentType,
		Size:         size,
	}
	var (
		file          *filesqlc.File
		webhookQueued bool
		err           error
	)
	if txDispatcher, ok := s.webhooks.(WebhookTxDispatcher); ok && s.txManager != nil {
		// The metadata row and the webhook outbox job commit together; an
		// enqueue failure rolls the row back and cleans up the blob below.
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			created, createErr := filesrepo.NewFileRepository(tx).Create(ctx, params)
			if createErr != nil {
				return createErr
			}
			if hookErr := dispatchWebhookTx(ctx, txDispatcher, tx, webhookcontracts.EventFileUploaded, tenantID, s.toFileResponse(created)); hookErr != nil {
				slog.Error("failed to enqueue file upload webhook", slog.Any("error", hookErr))
				return hookErr
			}
			file = created
			return nil
		})
		webhookQueued = err == nil
	} else {
		file, err = s.repo.Create(ctx, params)
	}
	if err != nil {
		// Cleanup storage on DB failure. The request context may already be
		// canceled; storage cleanup must still run.
		if delErr := s.storage.Delete(context.WithoutCancel(ctx), storagePath); delErr != nil {
			slog.Error("failed to cleanup storage after DB failure",
				slog.String("path", storagePath),
				slog.Any("error", delErr),
			)
		}
		if database.IsUniqueViolation(err) {
			return nil, apperror.NewConflict("a file with this storage path already exists")
		}
		return nil, apperror.NewInternal("failed to save file metadata")
	}

	response := s.toFileResponse(file)
	if !webhookQueued {
		s.dispatchWebhook(ctx, webhookcontracts.EventFileUploaded, tenantID, response)
	}
	return response, nil
}

// WebhookTxDispatcher is the optional transactional side of the webhooks
// dispatcher. It cannot live in the stdlib-only contracts package because it
// takes the producer's pgx.Tx.
type WebhookTxDispatcher interface {
	DispatchTx(ctx context.Context, tx pgx.Tx, events ...webhookcontracts.Event) error
}

// AttachTxManager enables transactional webhook dispatch: when the wired
// dispatcher implements WebhookTxDispatcher, file mutations and their webhook
// outbox jobs commit atomically.
func AttachTxManager(svc UploadService, txManager *database.TxManager) {
	if s, ok := svc.(*uploadService); ok {
		s.txManager = txManager
	}
}

// dispatchWebhookTx enqueues one domain event inside tx.
func dispatchWebhookTx(ctx context.Context, dispatcher WebhookTxDispatcher, tx pgx.Tx, eventType string, tenantID int64, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}
	return dispatcher.DispatchTx(ctx, tx, webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	})
}

// SetWebhookDispatcher wires outbound domain events.
func (s *uploadService) SetWebhookDispatcher(dispatcher webhookcontracts.Dispatcher) {
	s.webhooks = dispatcher
}

// AttachWebhookDispatcher wires the production dispatcher without widening the
// UploadService interface, so handler fakes remain small.
func AttachWebhookDispatcher(svc UploadService, dispatcher webhookcontracts.Dispatcher) {
	if s, ok := svc.(*uploadService); ok {
		s.webhooks = dispatcher
	}
}

// dispatchWebhook emits one best-effort domain event. Delivery failures are
// logged; they must not turn a committed file mutation into an API error.
func (s *uploadService) dispatchWebhook(ctx context.Context, eventType string, tenantID int64, data any) {
	if s.webhooks == nil {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Warn("failed to marshal webhook payload", slog.String("event", eventType), slog.Any("error", err))
		return
	}
	if _, err := s.webhooks.Dispatch(context.WithoutCancel(ctx), webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	}); err != nil {
		slog.Warn("failed to dispatch webhook event", slog.String("event", eventType), slog.Any("error", err))
	}
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
	if err := authorizeFile(ctx, filescatalog.PermissionFilesRead, file); err != nil {
		return nil, err
	}

	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
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

	if err := authorizeFile(ctx, filescatalog.PermissionFilesRead, file); err != nil {
		return nil, nil, err
	}
	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
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

	if err := authorizeFile(ctx, filescatalog.PermissionFilesDelete, file); err != nil {
		return err
	}
	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
		return apperror.NewNotFound("file not found")
	}

	eventData := map[string]any{
		"id":           file.ID,
		"tenantId":     file.TenantID,
		"userId":       file.UserID,
		"originalName": file.OriginalName,
	}
	// Soft delete — do NOT remove from storage so the file can be restored.
	webhookQueued := false
	if txDispatcher, ok := s.webhooks.(WebhookTxDispatcher); ok && s.txManager != nil {
		// The soft delete and its webhook outbox job commit together.
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			if _, delErr := filesrepo.NewFileRepository(tx).Delete(ctx, id); delErr != nil {
				return delErr
			}
			if hookErr := dispatchWebhookTx(ctx, txDispatcher, tx, webhookcontracts.EventFileDeleted, file.TenantID, eventData); hookErr != nil {
				slog.Error("failed to enqueue file delete webhook", slog.Int64("file_id", id), slog.Any("error", hookErr))
				return hookErr
			}
			return nil
		})
		webhookQueued = err == nil
	} else {
		_, err = s.repo.Delete(ctx, id)
	}
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
	if !webhookQueued {
		s.dispatchWebhook(ctx, webhookcontracts.EventFileDeleted, file.TenantID, eventData)
	}

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
	if err := authorizeFile(ctx, filescatalog.PermissionFilesRestore, file); err != nil {
		return nil, err
	}
	if file.TenantID != tenant.EffectiveID(ctx) || file.UserID != userID {
		return nil, apperror.NewNotFound("file not found")
	}
	if !file.DeletedAt.Valid {
		return nil, apperror.NewCode(filescatalog.CodeFileNotDeleted, "file is not deleted")
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

func (s *uploadService) toFileResponse(file *filesqlc.File) *dto.FileResponse {
	resp := &dto.FileResponse{
		ID:           file.ID,
		TenantID:     file.TenantID,
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
