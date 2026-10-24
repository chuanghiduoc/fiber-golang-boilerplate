package repository

import (
	"context"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

type EmailVerificationRepository interface {
	Create(ctx context.Context, params identitysqlc.CreateEmailVerificationTokenParams) (*identitysqlc.EmailVerificationToken, error)
	GetByToken(ctx context.Context, token string) (*identitysqlc.EmailVerificationToken, error)
	// Consume atomically deletes and returns a token so a verification link
	// cannot be replayed by two concurrent requests.
	Consume(ctx context.Context, token string) (*identitysqlc.EmailVerificationToken, error)
	Delete(ctx context.Context, token string) error
	DeleteByUserID(ctx context.Context, userID int64) error
}

type emailVerificationRepository struct {
	q *identitysqlc.Queries
}

func NewEmailVerificationRepository(db identitysqlc.DBTX) EmailVerificationRepository {
	return &emailVerificationRepository{q: identitysqlc.New(db)}
}

func (r *emailVerificationRepository) Create(ctx context.Context, params identitysqlc.CreateEmailVerificationTokenParams) (*identitysqlc.EmailVerificationToken, error) {
	rt, err := r.q.CreateEmailVerificationToken(ctx, params)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *emailVerificationRepository) GetByToken(ctx context.Context, token string) (*identitysqlc.EmailVerificationToken, error) {
	rt, err := r.q.GetEmailVerificationTokenByToken(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &rt, nil
}

func (r *emailVerificationRepository) Consume(ctx context.Context, token string) (*identitysqlc.EmailVerificationToken, error) {
	vt, err := r.q.ConsumeEmailVerificationToken(ctx, token)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &vt, nil
}

func (r *emailVerificationRepository) Delete(ctx context.Context, token string) error {
	return r.q.DeleteEmailVerificationToken(ctx, token)
}

func (r *emailVerificationRepository) DeleteByUserID(ctx context.Context, userID int64) error {
	return r.q.DeleteEmailVerificationTokensByUserID(ctx, userID)
}
