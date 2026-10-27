package repository

import (
	"context"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// FileRepository implements service.FileRepository over sqlc. Every method
// scopes its query to the request tenant resolved from ctx and maps sqlc rows
// to service types.
type FileRepository struct {
	q *filesqlc.Queries
}

var _ filesvc.FileRepository = (*FileRepository)(nil)

// NewFileRepository binds the repository to db: a pool for standalone reads
// and writes, or a pgx.Tx inside Transactor.InTx.
func NewFileRepository(db filesqlc.DBTX) *FileRepository {
	return &FileRepository{q: filesqlc.New(db)}
}

func (r *FileRepository) Create(ctx context.Context, file filesvc.NewFile) (*filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// The request tenant is authoritative; the input has no tenant field.
	row, err := r.q.CreateFile(ctx, filesqlc.CreateFileParams{
		TenantID:     tenantID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		StoragePath:  file.StoragePath,
		MimeType:     file.MimeType,
		Size:         file.Size,
	})
	if err != nil {
		if database.IsUniqueViolation(err) {
			return nil, filesvc.ErrStoragePathTaken
		}
		return nil, wrapErr(err)
	}
	return toFile(row), nil
}

func (r *FileRepository) GetByID(ctx context.Context, id uuid.UUID) (*filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return oneFile(r.q.GetFileByID(ctx, filesqlc.GetFileByIDParams{
		ID:       id,
		TenantID: tenantID,
	}))
}

func (r *FileRepository) GetByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (*filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return oneFile(r.q.GetFileByIDIncludingDeleted(ctx, filesqlc.GetFileByIDIncludingDeletedParams{
		ID:       id,
		TenantID: tenantID,
	}))
}

func (r *FileRepository) ListByUserCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor, includeDeleted bool) ([]filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return manyFiles(r.q.ListFilesByUserCursor(ctx, filesqlc.ListFilesByUserCursorParams{
		TenantID:        tenantID,
		UserID:          userID,
		IncludeDeleted:  includeDeleted,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	}))
}

func (r *FileRepository) AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return manyFiles(r.q.AdminListFilesCursor(ctx, filesqlc.AdminListFilesCursorParams{
		TenantID:        tenantID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	}))
}

func (r *FileRepository) ListCursor(ctx context.Context, cur pagination.DBCursor, includeDeleted bool) ([]filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return manyFiles(r.q.ListFilesCursor(ctx, filesqlc.ListFilesCursorParams{
		TenantID:        tenantID,
		IncludeDeleted:  includeDeleted,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	}))
}

func (r *FileRepository) Delete(ctx context.Context, id uuid.UUID) (*filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return oneFile(r.q.DeleteFile(ctx, filesqlc.DeleteFileParams{
		ID:       id,
		TenantID: tenantID,
	}))
}

func (r *FileRepository) Restore(ctx context.Context, id uuid.UUID) (*filesvc.File, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return oneFile(r.q.RestoreFile(ctx, filesqlc.RestoreFileParams{
		ID:       id,
		TenantID: tenantID,
	}))
}

func (r *FileRepository) AdminFileStats(ctx context.Context) (filesvc.FileStats, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return filesvc.FileStats{}, err
	}
	stats, err := r.q.AdminFileStats(ctx, tenantID)
	if err != nil {
		return filesvc.FileStats{}, err
	}
	return filesvc.FileStats{TotalFiles: stats.TotalFiles, TotalFileSize: stats.TotalFileSize}, nil
}

// LockQuota takes the per-tenant advisory lock that serializes uploads. The
// lock is transaction-scoped: the upload service calls this inside its
// metadata transaction, and the lock releases with the commit or rollback.
func (r *FileRepository) LockQuota(ctx context.Context) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	return r.q.LockFilesQuota(ctx, tenantID)
}

func (r *FileRepository) StorageUsage(ctx context.Context, userID uuid.UUID) (tenantBytes, userBytes int64, err error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return 0, 0, err
	}
	usage, err := r.q.FilesStorageUsage(ctx, filesqlc.FilesStorageUsageParams{
		TenantID: tenantID,
		UserID:   userID,
	})
	if err != nil {
		return 0, 0, err
	}
	return usage.TenantBytes, usage.UserBytes, nil
}

// ExistingStoragePaths returns which of paths have a metadata row in the
// request tenant. The orphan sweep calls it once per tenant batch.
func (r *FileRepository) ExistingStoragePaths(ctx context.Context, paths []string) (map[string]struct{}, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListFileStoragePaths(ctx, filesqlc.ListFileStoragePathsParams{
		TenantID: tenantID,
		Paths:    paths,
	})
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(rows))
	for _, path := range rows {
		existing[path] = struct{}{}
	}
	return existing, nil
}

// oneFile maps a single-row query result.
func oneFile(row filesqlc.File, err error) (*filesvc.File, error) {
	if err != nil {
		return nil, wrapErr(err)
	}
	return toFile(row), nil
}

// manyFiles maps a multi-row query result.
func manyFiles(rows []filesqlc.File, err error) ([]filesvc.File, error) {
	if err != nil {
		return nil, err
	}
	files := make([]filesvc.File, len(rows))
	for i := range rows {
		files[i] = *toFile(rows[i])
	}
	return files, nil
}

// toFile maps a sqlc row to the service type. updated_at is not exposed.
func toFile(row filesqlc.File) *filesvc.File {
	return &filesvc.File{
		ID:           row.ID,
		TenantID:     row.TenantID,
		UserID:       row.UserID,
		OriginalName: row.OriginalName,
		StoragePath:  row.StoragePath,
		MimeType:     row.MimeType,
		Size:         row.Size,
		CreatedAt:    row.CreatedAt.Time,
		DeletedAt:    database.TimePtr(row.DeletedAt),
	}
}

// wrapErr translates pgx errors to app-level sentinel errors without pulling
// in a shared repository package.
func wrapErr(err error) error {
	if mapped := database.MapError(err); mapped != err {
		return mapped
	}
	return err
}
