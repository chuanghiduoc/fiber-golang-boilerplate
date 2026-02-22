package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

type FileRepository interface {
	Create(ctx context.Context, params sqlc.CreateFileParams) (*sqlc.File, error)
	GetByID(ctx context.Context, id int64) (*sqlc.File, error)
	ListByUserID(ctx context.Context, userID int64, limit, offset int32) ([]sqlc.File, error)
	CountByUserID(ctx context.Context, userID int64) (int64, error)
	Delete(ctx context.Context, id int64) (*sqlc.File, error)
	Restore(ctx context.Context, id int64) (*sqlc.File, error)
	AdminList(ctx context.Context, limit, offset int32) ([]sqlc.File, error)
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

func (r *fileRepository) ListByUserID(ctx context.Context, userID int64, limit, offset int32) ([]sqlc.File, error) {
	return r.q.ListFilesByUserID(ctx, sqlc.ListFilesByUserIDParams{
		UserID: userID,
		Limit:  limit,
		Offset: offset,
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

func (r *fileRepository) AdminCount(ctx context.Context) (int64, error) {
	return r.q.AdminCountFiles(ctx)
}
