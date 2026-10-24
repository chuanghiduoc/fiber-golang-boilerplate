package repository

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// FileRepository is the files module's data access contract.
type FileRepository interface {
	Create(ctx context.Context, params sqlc.CreateFileParams) (*sqlc.File, error)
	GetByID(ctx context.Context, id int64) (*sqlc.File, error)
	GetByIDIncludingDeleted(ctx context.Context, id int64) (*sqlc.File, error)
	ListByUserID(ctx context.Context, userID int64, limit, offset int32) ([]sqlc.File, error)
	ListByUserCursor(ctx context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]sqlc.File, error)
	CountByUserID(ctx context.Context, userID int64) (int64, error)
	Delete(ctx context.Context, id int64) (*sqlc.File, error)
	Restore(ctx context.Context, id int64) (*sqlc.File, error)
	AdminList(ctx context.Context, limit, offset int32) ([]sqlc.File, error)
	AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]sqlc.File, error)
	AdminCount(ctx context.Context) (int64, error)
}

type fileRepository struct {
	q *sqlc.Queries
}

func NewFileRepository(db sqlc.DBTX) FileRepository {
	return &fileRepository{
		q: sqlc.New(db),
	}
}

func (r *fileRepository) Create(ctx context.Context, params sqlc.CreateFileParams) (*sqlc.File, error) {
	file, err := r.q.CreateFile(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) GetByID(ctx context.Context, id int64) (*sqlc.File, error) {
	file, err := r.q.GetFileByID(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) GetByIDIncludingDeleted(ctx context.Context, id int64) (*sqlc.File, error) {
	file, err := r.q.GetFileByIDIncludingDeleted(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) ListByUserID(ctx context.Context, userID int64, limit, offset int32) ([]sqlc.File, error) {
	return r.q.ListFilesByUserID(ctx, sqlc.ListFilesByUserIDParams{
		UserID: userID,
		Limit:  limit,
		Offset: offset,
	})
}

func (r *fileRepository) ListByUserCursor(ctx context.Context, userID int64, cur pagination.DBCursor, includeDeleted bool) ([]sqlc.File, error) {
	return r.q.ListFilesByUserCursor(ctx, sqlc.ListFilesByUserCursorParams{
		UserID:          userID,
		IncludeDeleted:  includeDeleted,
		HasCursor:       cur.HasCursor,
		CursorCreatedAt: cursorCreatedAt(cur),
		CursorID:        cur.ID,
		RowLimit:        cur.Limit,
	})
}

func (r *fileRepository) CountByUserID(ctx context.Context, userID int64) (int64, error) {
	return r.q.CountFilesByUserID(ctx, userID)
}

func (r *fileRepository) Delete(ctx context.Context, id int64) (*sqlc.File, error) {
	file, err := r.q.DeleteFile(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) Restore(ctx context.Context, id int64) (*sqlc.File, error) {
	file, err := r.q.RestoreFile(ctx, id)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &file, nil
}

func (r *fileRepository) AdminList(ctx context.Context, limit, offset int32) ([]sqlc.File, error) {
	return r.q.AdminListFiles(ctx, sqlc.AdminListFilesParams{
		Limit:  limit,
		Offset: offset,
	})
}

func (r *fileRepository) AdminListCursor(ctx context.Context, cur pagination.DBCursor) ([]sqlc.File, error) {
	return r.q.AdminListFilesCursor(ctx, sqlc.AdminListFilesCursorParams{
		HasCursor:       cur.HasCursor,
		CursorCreatedAt: cursorCreatedAt(cur),
		CursorID:        cur.ID,
		RowLimit:        cur.Limit,
	})
}

func (r *fileRepository) AdminCount(ctx context.Context) (int64, error) {
	return r.q.AdminCountFiles(ctx)
}

func cursorCreatedAt(cur pagination.DBCursor) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: cur.CreatedAt, Valid: cur.HasCursor}
}

// wrapErr translates pgx errors to app-level sentinel errors. Keeping it local
// avoids the module depending on the legacy internal/repository package.
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

// IsUniqueViolation reports whether err is a PostgreSQL unique constraint
// violation (SQLSTATE 23505).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
