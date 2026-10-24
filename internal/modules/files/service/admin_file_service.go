package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// AdminFileService owns the files side of the back-office surface: listing all
// metadata (including soft-deleted) for the request tenant, admin downloads,
// takedowns, restores, and the files half of the dashboard statistics.
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
	repo      repository.FileRepository
	txManager *database.TxManager
	audit     audit.TxRecorder
	storage   storage.Storage
	webhooks  webhookcontracts.Dispatcher
}

// AttachAdminWebhookDispatcher wires outbound file.deleted/file.restored
// events into admin takedowns and restores without widening the
// AdminFileService interface.
func AttachAdminWebhookDispatcher(svc AdminFileService, dispatcher webhookcontracts.Dispatcher) {
	if s, ok := svc.(*adminFileService); ok {
		s.webhooks = dispatcher
	}
}

// dispatchInTx enqueues the event inside tx when a transactional dispatcher is
// wired, reporting whether it did; the caller falls back to a post-commit
// dispatch otherwise. An enqueue failure fails the mutation (fail-closed).
func (s *adminFileService) dispatchInTx(ctx context.Context, tx pgx.Tx, eventType string, file *filesqlc.File) (bool, error) {
	txDispatcher, ok := s.webhooks.(WebhookTxDispatcher)
	if !ok {
		return false, nil
	}
	if err := dispatchWebhookTx(ctx, txDispatcher, tx, eventType, file.TenantID, fileEventData(file)); err != nil {
		slog.Error("failed to enqueue admin file webhook",
			slog.String("event", eventType),
			slog.Int64("file_id", file.ID),
			slog.Any("error", err),
		)
		return false, apperror.NewInternal("failed to enqueue file webhook")
	}
	return true, nil
}

func NewAdminFileService(
	repo repository.FileRepository,
	txManager *database.TxManager,
	auditRecorder audit.TxRecorder,
	store storage.Storage,
) AdminFileService {
	return &adminFileService{
		repo:      repo,
		txManager: txManager,
		audit:     auditRecorder,
		storage:   store,
	}
}

func (s *adminFileService) ListFiles(ctx context.Context, limit int, startingAfter string) ([]dto.FileResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	if err := authorizeTenant(ctx, authz.PermissionAdminFilesRead, tenant.EffectiveID(ctx)); err != nil {
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
	if err := authorizeTenant(ctx, authz.PermissionAdminFilesRead, file.TenantID); err != nil {
		return nil, nil, err
	}
	if file.TenantID != tenant.EffectiveID(ctx) {
		return nil, nil, apperror.NewNotFound("file not found")
	}

	reader, err := s.storage.Get(context.WithoutCancel(ctx), file.StoragePath)
	if err != nil {
		return nil, nil, storageReadError(file, err)
	}
	return s.toAdminFileResponse(file), reader, nil
}

func (s *adminFileService) DeleteFile(ctx context.Context, id int64) error {
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found or already deleted")
		}
		return apperror.NewInternal("failed to get file")
	}
	if err := authorizeTenant(ctx, authz.PermissionAdminFilesDelete, existing.TenantID); err != nil {
		return err
	}

	_, err = s.commitAdminMutation(ctx, adminFileMutation{
		id:           id,
		auditAction:  "admin.file.deleted",
		webhookEvent: webhookcontracts.EventFileDeleted,
		apply: func(repo repository.FileRepository) (*filesqlc.File, error) {
			file, err := repo.Delete(ctx, id)
			if err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return nil, apperror.NewNotFound("file not found or already deleted")
				}
				return nil, apperror.NewInternal("failed to delete file")
			}
			return file, nil
		},
	})
	return err
}

// adminFileMutation is one audited admin change to a file row.
type adminFileMutation struct {
	id           int64
	auditAction  string
	webhookEvent string
	apply        func(repository.FileRepository) (*filesqlc.File, error)
}

// commitAdminMutation applies m with its Tier 1 audit row and webhook. With a
// transaction manager the row change, audit row, and webhook outbox job commit
// together (an audit failure rolls the change back); a webhook that could not
// be queued in the transaction is dispatched after commit.
func (s *adminFileService) commitAdminMutation(ctx context.Context, m adminFileMutation) (*filesqlc.File, error) {
	auditEvent := func() audit.Event {
		event := audit.NewServiceEvent(ctx, m.auditAction, "file")
		event.ResourceID = strconv.FormatInt(m.id, 10)
		return event
	}

	if s.txManager == nil {
		file, err := m.apply(s.repo)
		if err != nil {
			return nil, err
		}
		if auditErr := audit.RecordInTx(ctx, s.audit, nil, auditEvent()); auditErr != nil {
			return nil, filesAuditWriteError(auditErr)
		}
		dispatchWebhookAfterCommit(ctx, s.webhooks, m.webhookEvent, file.TenantID, fileEventData(file))
		return file, nil
	}

	var (
		changed *filesqlc.File
		queued  bool
	)
	err := s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		file, applyErr := m.apply(repository.NewFileRepository(tx))
		if applyErr != nil {
			return applyErr
		}
		if auditErr := audit.RecordInTx(ctx, s.audit, tx, auditEvent()); auditErr != nil {
			return filesAuditWriteError(auditErr)
		}
		var hookErr error
		queued, hookErr = s.dispatchInTx(ctx, tx, m.webhookEvent, file)
		changed = file
		return hookErr
	})
	if err != nil {
		return nil, err
	}
	if !queued {
		dispatchWebhookAfterCommit(ctx, s.webhooks, m.webhookEvent, changed.TenantID, fileEventData(changed))
	}
	return changed, nil
}

func (s *adminFileService) RestoreFile(ctx context.Context, id int64) (*dto.FileResponse, error) {
	existing, err := s.repo.GetByIDIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.NewInternal("failed to get file")
	}
	if err := authorizeTenant(ctx, authz.PermissionAdminFilesRestore, existing.TenantID); err != nil {
		return nil, err
	}
	if existing.TenantID != tenant.EffectiveID(ctx) {
		return nil, apperror.NewNotFound("file not found")
	}
	if !existing.DeletedAt.Valid {
		return nil, apperror.NewCode(filescatalog.CodeFileNotDeleted, "file is not deleted")
	}

	restored, err := s.commitAdminMutation(ctx, adminFileMutation{
		id:           id,
		auditAction:  "admin.file.restored",
		webhookEvent: webhookcontracts.EventFileRestored,
		apply: func(repo repository.FileRepository) (*filesqlc.File, error) {
			file, err := repo.Restore(ctx, id)
			if err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return nil, apperror.NewNotFound("file not found or not deleted")
				}
				return nil, apperror.NewInternal("failed to restore file")
			}
			return file, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return s.toAdminFileResponse(restored), nil
}

func (s *adminFileService) GetFileStats(ctx context.Context) (FileStats, error) {
	if err := authorizeTenant(ctx, authz.PermissionAdminStatsRead, tenant.EffectiveID(ctx)); err != nil {
		return FileStats{}, err
	}
	stats, err := s.repo.AdminFileStats(ctx)
	if err != nil {
		return FileStats{}, apperror.NewInternal("failed to get file stats")
	}
	return FileStats{
		TotalFiles:    stats.TotalFiles,
		TotalFileSize: stats.TotalFileSize,
	}, nil
}
