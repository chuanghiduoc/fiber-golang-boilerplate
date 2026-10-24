package repository

import (
	"context"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

type PasswordResetRepository interface {
	Create(ctx context.Context, params identitysqlc.CreatePasswordResetTokenParams) (*identitysqlc.PasswordResetToken, error)
	GetByToken(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error)
	GetByTokenForUpdate(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error)
	Consume(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error)
	Delete(ctx context.Context, token string) error
	DeleteByUserID(ctx context.Context, userID int64) error
}

type passwordResetRepository struct {
	q *identitysqlc.Queries
}

func NewPasswordResetRepository(db identitysqlc.DBTX) PasswordResetRepository {
	return &passwordResetRepository{q: identitysqlc.New(db)}
}

func (r *passwordResetRepository) Create(ctx context.Context, params identitysqlc.CreatePasswordResetTokenParams) (*identitysqlc.PasswordResetToken, error) {
	rt, err := r.q.CreatePasswordResetToken(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *passwordResetRepository) GetByToken(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error) {
	rt, err := r.q.GetPasswordResetTokenByToken(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *passwordResetRepository) GetByTokenForUpdate(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error) {
	rt, err := r.q.GetPasswordResetTokenByTokenForUpdate(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *passwordResetRepository) Consume(ctx context.Context, token string) (*identitysqlc.PasswordResetToken, error) {
	rt, err := r.q.ConsumePasswordResetToken(ctx, token)
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
