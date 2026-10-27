package service

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// AdminFileService owns the files side of the back-office surface: listing all
// metadata (including soft-deleted) for the request tenant, admin downloads,
// takedowns, restores, and the files half of the dashboard statistics.
type AdminFileService interface {
	ListFiles(ctx context.Context, limit int, startingAfter string) ([]File, bool, error)
	Download(ctx context.Context, id uuid.UUID) (*File, io.ReadCloser, error)
	DeleteFile(ctx context.Context, id uuid.UUID) error
	RestoreFile(ctx context.Context, id uuid.UUID) (*File, error)
	GetFileStats(ctx context.Context) (FileStats, error)
}

type adminFileService struct {
	repo       FileRepository
	tx         Transactor
	storage    storage.Storage
	authorizer authz.Authorizer
}

// NewAdminFileService builds the admin file service. Takedowns and restores
// run through tx with their Tier 1 audit row and domain event.
func NewAdminFileService(repo FileRepository, tx Transactor, store storage.Storage, authorizer authz.Authorizer) AdminFileService {
	return &adminFileService{repo: repo, tx: tx, storage: store, authorizer: authorizer}
}

func (s *adminFileService) ListFiles(ctx context.Context, limit int, startingAfter string) ([]File, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, false, apperror.Internal(err, "tenant context is required")
	}
	if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesRead, tenantID); err != nil {
		return nil, false, err
	}

	files, err := s.repo.AdminListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.Internal(err, "failed to list files")
	}

	hasMore := len(files) > pageSize
	if hasMore {
		files = files[:pageSize]
	}
	return files, hasMore, nil
}

func (s *adminFileService) Download(ctx context.Context, id uuid.UUID) (*File, io.ReadCloser, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, apperror.NewNotFound("file not found")
		}
		return nil, nil, apperror.Internal(err, "failed to get file")
	}
	if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesRead, file.TenantID); err != nil {
		return nil, nil, err
	}
	if file.TenantID != tenant.IDFromContext(ctx) {
		return nil, nil, apperror.NewNotFound("file not found")
	}

	reader, err := s.storage.Get(context.WithoutCancel(ctx), file.StoragePath)
	if err != nil {
		return nil, nil, storageReadError(file, err)
	}
	return file, reader, nil
}

func (s *adminFileService) DeleteFile(ctx context.Context, id uuid.UUID) error {
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found or already deleted")
		}
		return apperror.Internal(err, "failed to get file")
	}
	if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesDelete, existing.TenantID); err != nil {
		return err
	}

	_, err = s.commitAdminMutation(ctx, adminFileMutation{
		id:          id,
		auditAction: "admin.file.deleted",
		eventType:   filescontracts.EventFileDeleted,
		dataVersion: filescontracts.FileDeletedVersion,
		eventData:   func(file *File) any { return fileDeleted(file) },
		apply: func(ctx context.Context, repo FileRepository) (*File, error) {
			file, err := repo.Delete(ctx, id)
			if err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return nil, apperror.NewNotFound("file not found or already deleted")
				}
				return nil, apperror.Internal(err, "failed to delete file")
			}
			return file, nil
		},
	})
	return err
}

// adminFileMutation is one audited admin change to a file row.
type adminFileMutation struct {
	id          uuid.UUID
	auditAction string
	eventType   string
	dataVersion int
	eventData   func(*File) any
	apply       func(context.Context, FileRepository) (*File, error)
}

// commitAdminMutation applies m with its Tier 1 audit row and domain event.
// The row change, audit row, and event commit together: an audit or publish
// failure rolls the change back.
func (s *adminFileService) commitAdminMutation(ctx context.Context, m adminFileMutation) (*File, error) {
	var changed *File
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		file, applyErr := m.apply(ctx, tx.Files)
		if applyErr != nil {
			return applyErr
		}
		event := audit.NewServiceEvent(ctx, m.auditAction, "file")
		event.ResourceID = m.id.String()
		if auditErr := tx.Audit.RecordTx(ctx, event); auditErr != nil {
			return filesAuditWriteError(auditErr)
		}
		if pubErr := publishEvent(ctx, tx.Events, m.eventType, m.dataVersion, file.TenantID, m.eventData(file)); pubErr != nil {
			return apperror.Internal(pubErr, "failed to publish file event")
		}
		changed = file
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changed, nil
}

func (s *adminFileService) RestoreFile(ctx context.Context, id uuid.UUID) (*File, error) {
	existing, err := s.repo.GetByIDIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.Internal(err, "failed to get file")
	}
	if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesRestore, existing.TenantID); err != nil {
		return nil, err
	}
	if existing.TenantID != tenant.IDFromContext(ctx) {
		return nil, apperror.NewNotFound("file not found")
	}
	if existing.DeletedAt == nil {
		return nil, apperror.NewCode(filescatalog.CodeFileNotDeleted, "file is not deleted")
	}

	return s.commitAdminMutation(ctx, adminFileMutation{
		id:          id,
		auditAction: "admin.file.restored",
		eventType:   filescontracts.EventFileRestored,
		dataVersion: filescontracts.FileRestoredVersion,
		eventData:   func(file *File) any { return fileRestored(file) },
		apply: func(ctx context.Context, repo FileRepository) (*File, error) {
			file, err := repo.Restore(ctx, id)
			if err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return nil, apperror.NewNotFound("file not found or not deleted")
				}
				return nil, apperror.Internal(err, "failed to restore file")
			}
			return file, nil
		},
	})
}

func (s *adminFileService) GetFileStats(ctx context.Context) (FileStats, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return FileStats{}, apperror.Internal(err, "tenant context is required")
	}
	if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminStatsRead, tenantID); err != nil {
		return FileStats{}, err
	}
	stats, err := s.repo.AdminFileStats(ctx)
	if err != nil {
		return FileStats{}, apperror.Internal(err, "failed to get file stats")
	}
	return stats, nil
}
