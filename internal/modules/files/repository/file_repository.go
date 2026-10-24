package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// FileRepository is the files module's data access contract. Every method
// scopes its query to the request tenant resolved from ctx.
type FileRepository interface {
	Create(ctx context.Context, params filesqlc.CreateFileParams) (*filesqlc.File, error)
	GetByID(ctx context.Context, id int64) (*filesqlc.File, error)
	GetByIDIncludingDeleted(ctx context.Context, id int64) (*filesqlc.File, error)
	ListByUserID(ctx context.Context, userID int64, limit, offset int32) ([]filesqlc.File, error)
	ListByUserCursor(ctx context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]filesqlc.File, error)
	CountByUserID(ctx context.Context, userID int64) (int64, error)
	Delete(ctx context.Context, id int64) (*filesqlc.File, error)
	Restore(ctx context.Context, id int64) (*filesqlc.File, error)
	AdminList(ctx context.Context, limit, offset int32) ([]filesqlc.File, error)
	AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]filesqlc.File, error)
	AdminCount(ctx context.Context) (int64, error)
	AdminFileStats(ctx context.Context) (filesqlc.AdminFileStatsRow, error)
}

type fileRepository struct {
	q *filesqlc.Queries
}

func NewFileRepository(db filesqlc.DBTX) FileRepository {
	return &fileRepository{
		q: filesqlc.New(db),
	}
}

// tenantID returns the request tenant every repository query must scope to.
// EffectiveID falls back to the default tenant only for background jobs,
// internal callers, and tests; HTTP requests resolve an explicit tenant (and
// production requires X-Tenant-Slug).
func tenantID(ctx context.Context) int64 {
	return tenant.EffectiveID(ctx)
}

func (r *fileRepository) Create(ctx context.Context, params filesqlc.CreateFileParams) (*filesqlc.File, error) {
	// The request tenant is authoritative; callers cannot create a file on
	// behalf of another tenant even if they set the generated field.
	params.TenantID = tenantID(ctx)
	file, err := r.q.CreateFile(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) GetByID(ctx context.Context, id int64) (*filesqlc.File, error) {
	file, err := r.q.GetFileByID(ctx, filesqlc.GetFileByIDParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) GetByIDIncludingDeleted(ctx context.Context, id int64) (*filesqlc.File, error) {
	file, err := r.q.GetFileByIDIncludingDeleted(ctx, filesqlc.GetFileByIDIncludingDeletedParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) ListByUserID(ctx context.Context, userID int64, limit, offset int32) ([]filesqlc.File, error) {
	return r.q.ListFilesByUserID(ctx, filesqlc.ListFilesByUserIDParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
		Limit:    limit,
		Offset:   offset,
	})
}

func (r *fileRepository) ListByUserCursor(ctx context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]filesqlc.File, error) {
	return r.q.ListFilesByUserCursor(ctx, filesqlc.ListFilesByUserCursorParams{
		TenantID:        tenantID(ctx),
		UserID:          userID,
		IncludeDeleted:  includeDeleted,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
}

func (r *fileRepository) CountByUserID(ctx context.Context, userID int64) (int64, error) {
	return r.q.CountFilesByUserID(ctx, filesqlc.CountFilesByUserIDParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
	})
}

func (r *fileRepository) Delete(ctx context.Context, id int64) (*filesqlc.File, error) {
	file, err := r.q.DeleteFile(ctx, filesqlc.DeleteFileParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) Restore(ctx context.Context, id int64) (*filesqlc.File, error) {
	file, err := r.q.RestoreFile(ctx, filesqlc.RestoreFileParams{
		ID:       id,
		TenantID: tenantID(ctx),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) AdminList(ctx context.Context, limit, offset int32) ([]filesqlc.File, error) {
	return r.q.AdminListFiles(ctx, filesqlc.AdminListFilesParams{
		TenantID: tenantID(ctx),
		Limit:    limit,
		Offset:   offset,
	})
}

func (r *fileRepository) AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]filesqlc.File, error) {
	return r.q.AdminListFilesCursor(ctx, filesqlc.AdminListFilesCursorParams{
		TenantID:        tenantID(ctx),
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
}

func (r *fileRepository) AdminCount(ctx context.Context) (int64, error) {
	return r.q.AdminCountFiles(ctx, tenantID(ctx))
}

func (r *fileRepository) AdminFileStats(ctx context.Context) (filesqlc.AdminFileStatsRow, error) {
	return r.q.AdminFileStats(ctx, tenantID(ctx))
}

// wrapErr translates pgx errors to app-level sentinel errors without pulling
// in a shared repository package.
func wrapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperror.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "P0001" &&
		strings.Contains(pgErr.Message, "last active admin") {
		return apperror.NewConflict("cannot remove the last active admin")
	}
	return err
}
