package repository

import (
	"context"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

type RefreshTokenRepository interface {
	Create(ctx context.Context, params identitysqlc.CreateRefreshTokenParams) (*identitysqlc.RefreshToken, error)
	GetByToken(ctx context.Context, token string) (*identitysqlc.RefreshToken, error)
	// Consume atomically deletes and returns a token. Exactly one concurrent
	// caller can win, which prevents refresh-token replay/rotation races.
	Consume(ctx context.Context, token string) (*identitysqlc.RefreshToken, error)
	Delete(ctx context.Context, token string) error
	DeleteByUserID(ctx context.Context, userID int64) error
}

type refreshTokenRepository struct {
	q *identitysqlc.Queries
}

func NewRefreshTokenRepository(db identitysqlc.DBTX) RefreshTokenRepository {
	return &refreshTokenRepository{q: identitysqlc.New(db)}
}

func (r *refreshTokenRepository) Create(ctx context.Context, params identitysqlc.CreateRefreshTokenParams) (*identitysqlc.RefreshToken, error) {
	rt, err := r.q.CreateRefreshToken(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *refreshTokenRepository) GetByToken(ctx context.Context, token string) (*identitysqlc.RefreshToken, error) {
	rt, err := r.q.GetRefreshTokenByToken(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *refreshTokenRepository) Consume(ctx context.Context, token string) (*identitysqlc.RefreshToken, error) {
	rt, err := r.q.ConsumeRefreshToken(ctx, token)
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
