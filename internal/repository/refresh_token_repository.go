package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

type RefreshTokenRepository interface {
	Create(ctx context.Context, params sqlc.CreateRefreshTokenParams) (*sqlc.RefreshToken, error)
	GetByToken(ctx context.Context, token string) (*sqlc.RefreshToken, error)
	Delete(ctx context.Context, token string) error
	DeleteByUserID(ctx context.Context, userID int64) error
}

type refreshTokenRepository struct {
	q *sqlc.Queries
}

func NewRefreshTokenRepository(db sqlc.DBTX) RefreshTokenRepository {
	return &refreshTokenRepository{q: sqlc.New(db)}
}

func (r *refreshTokenRepository) Create(ctx context.Context, params sqlc.CreateRefreshTokenParams) (*sqlc.RefreshToken, error) {
	rt, err := r.q.CreateRefreshToken(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *refreshTokenRepository) GetByToken(ctx context.Context, token string) (*sqlc.RefreshToken, error) {
	rt, err := r.q.GetRefreshTokenByToken(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *refreshTokenRepository) Delete(ctx context.Context, token string) error {
	return r.q.DeleteRefreshToken(ctx, token)
}

func (r *refreshTokenRepository) DeleteByUserID(ctx context.Context, userID int64) error {
	return r.q.DeleteRefreshTokensByUserID(ctx, userID)
}
