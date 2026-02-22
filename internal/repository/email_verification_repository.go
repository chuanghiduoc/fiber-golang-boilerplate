package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
)

type EmailVerificationRepository interface {
	Create(ctx context.Context, params sqlc.CreateEmailVerificationTokenParams) (*sqlc.EmailVerificationToken, error)
	GetByToken(ctx context.Context, token string) (*sqlc.EmailVerificationToken, error)
	Delete(ctx context.Context, token string) error
	DeleteByUserID(ctx context.Context, userID int64) error
}

type emailVerificationRepository struct {
	q *sqlc.Queries
}

func NewEmailVerificationRepository(db sqlc.DBTX) EmailVerificationRepository {
	return &emailVerificationRepository{q: sqlc.New(db)}
}

func (r *emailVerificationRepository) Create(ctx context.Context, params sqlc.CreateEmailVerificationTokenParams) (*sqlc.EmailVerificationToken, error) {
	rt, err := r.q.CreateEmailVerificationToken(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *emailVerificationRepository) GetByToken(ctx context.Context, token string) (*sqlc.EmailVerificationToken, error) {
	rt, err := r.q.GetEmailVerificationTokenByToken(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *emailVerificationRepository) Delete(ctx context.Context, token string) error {
	return r.q.DeleteEmailVerificationToken(ctx, token)
}

func (r *emailVerificationRepository) DeleteByUserID(ctx context.Context, userID int64) error {
	return r.q.DeleteEmailVerificationTokensByUserID(ctx, userID)
}
