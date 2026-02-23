package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

type PasswordResetRepository interface {
	Create(ctx context.Context, params sqlc.CreatePasswordResetTokenParams) (*sqlc.PasswordResetToken, error)
	GetByToken(ctx context.Context, token string) (*sqlc.PasswordResetToken, error)
	GetByTokenForUpdate(ctx context.Context, token string) (*sqlc.PasswordResetToken, error)
	Delete(ctx context.Context, token string) error
	DeleteByUserID(ctx context.Context, userID int64) error
}

type passwordResetRepository struct {
	q *sqlc.Queries
}

func NewPasswordResetRepository(db sqlc.DBTX) PasswordResetRepository {
	return &passwordResetRepository{q: sqlc.New(db)}
}

func (r *passwordResetRepository) Create(ctx context.Context, params sqlc.CreatePasswordResetTokenParams) (*sqlc.PasswordResetToken, error) {
	rt, err := r.q.CreatePasswordResetToken(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *passwordResetRepository) GetByToken(ctx context.Context, token string) (*sqlc.PasswordResetToken, error) {
	rt, err := r.q.GetPasswordResetTokenByToken(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *passwordResetRepository) GetByTokenForUpdate(ctx context.Context, token string) (*sqlc.PasswordResetToken, error) {
	rt, err := r.q.GetPasswordResetTokenByTokenForUpdate(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *passwordResetRepository) Delete(ctx context.Context, token string) error {
	return r.q.DeletePasswordResetToken(ctx, token)
}

func (r *passwordResetRepository) DeleteByUserID(ctx context.Context, userID int64) error {
	return r.q.DeletePasswordResetTokensByUserID(ctx, userID)
}
