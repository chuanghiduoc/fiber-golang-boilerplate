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
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage/scan"
)

const (
	maxOriginalNameRunes = 255
	// storageCleanupTimeout bounds detached cleanup calls after the request is done.
	storageCleanupTimeout = 30 * time.Second
)

var mimeExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"application/pdf": ".pdf",
}

type UploadService interface {
	Upload(ctx context.Context, userID uuid.UUID, filename string, reader io.Reader, size int64, contentType string) (*File, error)
	GetFileInfo(ctx context.Context, id, userID uuid.UUID) (*File, error)
	Download(ctx context.Context, id, userID uuid.UUID) (*File, io.ReadCloser, error)
	List(ctx context.Context, userID uuid.UUID, limit int, startingAfter string, includeDeleted bool) ([]File, bool, error)
	Delete(ctx context.Context, id, userID uuid.UUID) error
	Restore(ctx context.Context, id, userID uuid.UUID) (*File, error)
}

type uploadService struct {
	repo       FileRepository
	tx         Transactor
	storage    storage.Storage
	authorizer authz.Authorizer
	opts       UploadOptions
}

// NewUploadService builds the user file service. Mutations run through tx, so
// a metadata change and its domain event commit atomically.
func NewUploadService(repo FileRepository, tx Transactor, store storage.Storage, authorizer authz.Authorizer, opts UploadOptions) UploadService {
	return &uploadService{repo: repo, tx: tx, storage: store, authorizer: authorizer, opts: opts}
}

func (s *uploadService) Upload(ctx context.Context, userID uuid.UUID, filename string, reader io.Reader, size int64, contentType string) (*File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, apperror.Internal(err, "tenant context is required")
	}
	if err := authorizeUserResource(s.authorizer, ctx, filescatalog.PermissionFilesWrite, tenantID, userID); err != nil {
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
	storagePath := fmt.Sprintf("%s/%s/%s%s", tenantID, userID, uuid.New().String(), extensionForMIME(contentType))

	// Cheap pre-check before the blob Put and clamd scan: an upload that is
	// already over quota is refused without spending storage or scanner work.
	// The locked check inside the metadata transaction remains authoritative.
	if err := s.precheckQuota(ctx, s.repo, userID, size); err != nil {
		return nil, err
	}

	if err := s.storage.Put(ctx, storagePath, reader, size, contentType); err != nil {
		return nil, apperror.Internal(err, "failed to store file")
	}

	// Scan the stored blob before any metadata is committed. A configured
	// scanner fails closed: an infected file is 422 and a scanner problem is
	// 503, and neither leaves the blob behind.
	if s.opts.Scanner != nil {
		if err := s.scanStored(ctx, storagePath, size); err != nil {
			s.removeBlob(ctx, storagePath, "failed to cleanup storage after a rejected scan")
			return nil, err
		}
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
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := s.checkQuota(ctx, tx.Files, userID, size); err != nil {
			return err
		}
		created, createErr := tx.Files.Create(ctx, input)
		if createErr != nil {
			return createErr
		}
		if pubErr := publishEvent(ctx, tx.Events, filescontracts.EventFileUploaded, filescontracts.FileUploadedVersion, tenantID, []uuid.UUID{created.UserID}, fileUploaded(created)); pubErr != nil {
			return pubErr
		}
		file = created
		return nil
	})
	if err != nil {
		// Cleanup storage on DB failure. The request context may already be
		// canceled; storage cleanup must still run.
		s.removeBlob(ctx, storagePath, "failed to cleanup storage after DB failure")
		if errors.Is(err, ErrStoragePathTaken) {
			return nil, apperror.NewConflict("a file with this storage path already exists")
		}
		var appErr *apperror.AppError
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, apperror.Internal(err, "failed to save file metadata")
	}
	return file, nil
}

// scanStored streams the stored blob through the configured scanner. The rows
// are not committed yet, so a bad verdict just removes the blob.
func (s *uploadService) scanStored(ctx context.Context, storagePath string, size int64) error {
	reader, err := s.storage.Get(ctx, storagePath)
	if err != nil {
		slog.Error("failed to read a stored upload for scanning",
			slog.String("path", storagePath),
			slog.Any("error", err),
		)
		return apperror.NewCode(filescatalog.CodeFileScanUnavailable, "file scanning is temporarily unavailable")
	}
	verdict, scanErr := s.opts.Scanner.Scan(ctx, reader, size)
	closeErr := reader.Close()
	if closeErr != nil {
		slog.Warn("failed to close scanned upload", slog.String("path", storagePath), slog.Any("error", closeErr))
	}
	if scanErr != nil {
		slog.Error("upload scan failed",
			slog.String("path", storagePath),
			slog.Any("error", scanErr),
		)
		return apperror.NewCode(filescatalog.CodeFileScanUnavailable, "file scanning is temporarily unavailable")
	}
	if verdict == scan.VerdictInfected {
		return apperror.NewCode(filescatalog.CodeFileInfected, "file failed the malware scan")
	}
	if verdict != scan.VerdictClean {
		slog.Error("upload scanner returned an unknown verdict", slog.String("verdict", verdict.String()))
		return apperror.NewCode(filescatalog.CodeFileScanUnavailable, "file scanning is temporarily unavailable")
	}
	return nil
}

// removeBlob deletes a stored blob whose upload was rejected. The request
// context may already be canceled, so cleanup uses a detached context, but it
// still needs a deadline: a stuck storage provider must not pin the request.
func (s *uploadService) removeBlob(ctx context.Context, storagePath, reason string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storageCleanupTimeout)
	defer cancel()
	if err := s.storage.Delete(cleanupCtx, storagePath); err != nil {
		slog.Error(reason, slog.String("path", storagePath), slog.Any("error", err))
	}
}

// precheckQuota is the cheap, lock-free quota rejection before a blob is
// written. It can race with a concurrent upload, which the locked check in the
// metadata transaction then catches; its only job is to spare clearly
// over-quota requests the Put and scan cost.
func (s *uploadService) precheckQuota(ctx context.Context, repo FileRepository, userID uuid.UUID, size int64) error {
	if !s.quotasEnabled() {
		return nil
	}
	usage, err := repo.StorageUsage(ctx, userID, s.quotaScope())
	if err != nil {
		slog.Error("failed to check storage quota before upload", slog.Any("error", err))
		return apperror.Internal(err, "failed to check storage quota")
	}
	if s.quotaExceeded(usage, size) {
		return apperror.NewCode(filescatalog.CodeFileQuotaExceeded, "storage quota exceeded")
	}
	return nil
}

func (s *uploadService) quotasEnabled() bool {
	return s.opts.TenantQuotaBytes > 0 || s.opts.UserQuotaBytes > 0
}

func (s *uploadService) quotaScope() QuotaScope {
	return QuotaScope{
		Tenant: s.opts.TenantQuotaBytes > 0,
		User:   s.opts.UserQuotaBytes > 0,
	}
}

func (s *uploadService) quotaExceeded(usage QuotaUsage, size int64) bool {
	tenantExceeded := s.opts.TenantQuotaBytes > 0 && usage.TenantBytes+size > s.opts.TenantQuotaBytes
	userExceeded := s.opts.UserQuotaBytes > 0 && usage.UserBytes+size > s.opts.UserQuotaBytes
	return tenantExceeded || userExceeded
}

// checkQuota refuses an upload that would push the tenant or the user over
// its configured byte quota. A user quota locks only that user's counter row;
// a tenant quota takes a tenant advisory lock and sums the per-user counters.
// The lock is held until the transaction ends, so concurrent uploads serialize
// exactly as far as the configured quota requires. Trashed rows count because
// their blobs are still stored.
func (s *uploadService) checkQuota(ctx context.Context, repo FileRepository, userID uuid.UUID, size int64) error {
	if !s.quotasEnabled() {
		return nil
	}
	usage, err := repo.LockStorageUsage(ctx, userID, s.quotaScope())
	if err != nil {
		return err
	}
	if !s.quotaExceeded(usage, size) {
		return nil
	}
	return apperror.NewCode(filescatalog.CodeFileQuotaExceeded, "storage quota exceeded")
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

// hasTenantFilePermission reports whether the caller holds the admin
// permission that widens the file collection to every owner in the tenant.
// Background callers have no subject and stay on the owner-scoped path.
func hasTenantFilePermission(ctx context.Context, permission authz.Permission) bool {
	subject, ok := authz.SubjectFromContext(ctx)
	return ok && subject.Has(permission)
}

func (s *uploadService) GetFileInfo(ctx context.Context, id, userID uuid.UUID) (*File, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.Internal(err, "failed to get file")
	}
	if hasTenantFilePermission(ctx, authz.PermissionAdminFilesRead) {
		if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesRead, file.TenantID); err != nil {
			return nil, err
		}
		if file.TenantID != tenant.IDFromContext(ctx) {
			// Do not reveal that another tenant's file ID exists.
			return nil, apperror.NewNotFound("file not found")
		}
		return file, nil
	}
	if err := authorizeFile(s.authorizer, ctx, filescatalog.PermissionFilesRead, file); err != nil {
		return nil, err
	}

	if file.TenantID != tenant.IDFromContext(ctx) || file.UserID != userID {
		// Do not reveal that another tenant's file ID exists.
		return nil, apperror.NewNotFound("file not found")
	}

	return file, nil
}

func (s *uploadService) Download(ctx context.Context, id, userID uuid.UUID) (*File, io.ReadCloser, error) {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, nil, apperror.NewNotFound("file not found")
		}
		return nil, nil, apperror.Internal(err, "failed to get file")
	}

	if hasTenantFilePermission(ctx, authz.PermissionAdminFilesRead) {
		if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesRead, file.TenantID); err != nil {
			return nil, nil, err
		}
		if file.TenantID != tenant.IDFromContext(ctx) {
			return nil, nil, apperror.NewNotFound("file not found")
		}
	} else {
		if err := authorizeFile(s.authorizer, ctx, filescatalog.PermissionFilesRead, file); err != nil {
			return nil, nil, err
		}
		if file.TenantID != tenant.IDFromContext(ctx) || file.UserID != userID {
			return nil, nil, apperror.NewNotFound("file not found")
		}
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
			slog.String("file_id", file.ID.String()),
			slog.String("tenant_id", file.TenantID.String()),
			slog.String("path", file.StoragePath),
		)
		return apperror.NewNotFound("file not found")
	}
	slog.Error("failed to read file from storage",
		slog.String("file_id", file.ID.String()),
		slog.Any("error", err),
	)
	return apperror.Internal(err, "failed to read file from storage")
}

func (s *uploadService) List(ctx context.Context, userID uuid.UUID, limit int, startingAfter string, includeDeleted bool) ([]File, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, false, apperror.Internal(err, "tenant context is required")
	}

	var files []File
	if hasTenantFilePermission(ctx, authz.PermissionAdminFilesRead) {
		if authErr := authorizeTenant(s.authorizer, ctx, filescatalog.PermissionFilesRead, tenantID); authErr != nil {
			return nil, false, authErr
		}
		if authErr := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesRead, tenantID); authErr != nil {
			return nil, false, authErr
		}
		files, err = s.repo.ListCursor(ctx, cur, includeDeleted)
	} else {
		if authErr := authorizeUserResource(s.authorizer, ctx, filescatalog.PermissionFilesRead, tenantID, userID); authErr != nil {
			return nil, false, authErr
		}
		files, err = s.repo.ListByUserCursor(ctx, userID, cur, includeDeleted)
	}
	if err != nil {
		return nil, false, apperror.Internal(err, "failed to list files")
	}

	hasMore := len(files) > pageSize
	if hasMore {
		files = files[:pageSize]
	}
	return files, hasMore, nil
}

func (s *uploadService) Delete(ctx context.Context, id, userID uuid.UUID) error {
	file, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found")
		}
		return apperror.Internal(err, "failed to get file")
	}

	if hasTenantFilePermission(ctx, authz.PermissionAdminFilesDelete) {
		if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesDelete, file.TenantID); err != nil {
			return err
		}
		if file.TenantID != tenant.IDFromContext(ctx) {
			return apperror.NewNotFound("file not found")
		}
	} else {
		if err := authorizeFile(s.authorizer, ctx, filescatalog.PermissionFilesDelete, file); err != nil {
			return err
		}
		if file.TenantID != tenant.IDFromContext(ctx) || file.UserID != userID {
			return apperror.NewNotFound("file not found")
		}
	}

	// Soft delete — do NOT remove from storage so the file can be restored.
	// The soft delete and its file.deleted event commit together.
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		deleted, delErr := tx.Files.Delete(ctx, id)
		if delErr != nil {
			return delErr
		}
		return publishEvent(ctx, tx.Events, filescontracts.EventFileDeleted, filescontracts.FileDeletedVersion, deleted.TenantID, []uuid.UUID{deleted.UserID}, fileDeleted(deleted))
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("file not found")
		}
		var appErr *apperror.AppError
		if errors.As(err, &appErr) {
			return appErr
		}
		return apperror.Internal(err, "failed to delete file metadata")
	}

	slog.Info("file soft-deleted",
		slog.String("file_id", id.String()),
		slog.String("path", file.StoragePath),
	)
	return nil
}

func (s *uploadService) Restore(ctx context.Context, id, userID uuid.UUID) (*File, error) {
	file, err := s.repo.GetByIDIncludingDeleted(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		return nil, apperror.Internal(err, "failed to get file")
	}
	if hasTenantFilePermission(ctx, authz.PermissionAdminFilesRestore) {
		if err := authorizeTenant(s.authorizer, ctx, authz.PermissionAdminFilesRestore, file.TenantID); err != nil {
			return nil, err
		}
		if file.TenantID != tenant.IDFromContext(ctx) {
			return nil, apperror.NewNotFound("file not found")
		}
	} else {
		if err := authorizeFile(s.authorizer, ctx, filescatalog.PermissionFilesRestore, file); err != nil {
			return nil, err
		}
		if file.TenantID != tenant.IDFromContext(ctx) || file.UserID != userID {
			return nil, apperror.NewNotFound("file not found")
		}
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
		if pubErr := publishEvent(ctx, tx.Events, filescontracts.EventFileRestored, filescontracts.FileRestoredVersion, row.TenantID, []uuid.UUID{row.UserID}, fileRestored(row)); pubErr != nil {
			return pubErr
		}
		restored = row
		return nil
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("file not found")
		}
		var appErr *apperror.AppError
		if errors.As(err, &appErr) {
			return nil, appErr
		}
		return nil, apperror.Internal(err, "failed to restore file")
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
