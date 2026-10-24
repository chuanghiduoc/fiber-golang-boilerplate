package repository

import (
	"context"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

// TokenPurgeRepository deletes expired identity tokens. Unlike every other
// identity query these run across all tenants on purpose: retention is a
// maintenance job, and an expired token is unusable in any tenant. Each call
// deletes at most batchSize rows and returns how many it deleted.
type TokenPurgeRepository interface {
	PurgeExpiredRefreshTokens(ctx context.Context, batchSize int32) (int64, error)
	PurgeExpiredPasswordResetTokens(ctx context.Context, batchSize int32) (int64, error)
	PurgeExpiredEmailVerificationTokens(ctx context.Context, batchSize int32) (int64, error)
}

type tokenPurgeRepository struct {
	q *identitysqlc.Queries
}

func NewTokenPurgeRepository(db identitysqlc.DBTX) TokenPurgeRepository {
	return &tokenPurgeRepository{q: identitysqlc.New(db)}
}

func (r *tokenPurgeRepository) PurgeExpiredRefreshTokens(ctx context.Context, batchSize int32) (int64, error) {
	return r.q.PurgeExpiredRefreshTokens(ctx, batchSize)
}

func (r *tokenPurgeRepository) PurgeExpiredPasswordResetTokens(ctx context.Context, batchSize int32) (int64, error) {
	return r.q.PurgeExpiredPasswordResetTokens(ctx, batchSize)
}

func (r *tokenPurgeRepository) PurgeExpiredEmailVerificationTokens(ctx context.Context, batchSize int32) (int64, error) {
	return r.q.PurgeExpiredEmailVerificationTokens(ctx, batchSize)
}
