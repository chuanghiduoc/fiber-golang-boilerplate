package repository

import (
	"context"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/google/uuid"
)

// PasswordResetRepository implements core.PasswordResetRepository. Reset
// tokens are per user (global), so no query takes a tenant.
type PasswordResetRepository struct {
	q *identitysqlc.Queries
}

var _ core.PasswordResetRepository = (*PasswordResetRepository)(nil)

func NewPasswordResetRepository(db identitysqlc.DBTX) *PasswordResetRepository {
	return &PasswordResetRepository{q: identitysqlc.New(db)}
}

func toResetToken(row identitysqlc.PasswordResetToken) *core.PasswordResetToken {
	return &core.PasswordResetToken{
		ID:        row.ID,
		UserID:    row.UserID,
		ExpiresAt: row.ExpiresAt.Time,
		CreatedAt: row.CreatedAt.Time,
	}
}

func (r *PasswordResetRepository) Create(ctx context.Context, userID uuid.UUID, email string, tokenHash []byte, expiresAt time.Time) (bool, error) {
	_, err := r.q.CreatePasswordResetToken(ctx, identitysqlc.CreatePasswordResetTokenParams{
		UserID:    userID,
		Email:     email,
		TokenHash: tokenHash,
		ExpiresAt: database.Timestamptz(expiresAt),
	})
	if err != nil {
		if err = wrapErr(err); isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *PasswordResetRepository) GetByHash(ctx context.Context, tokenHash []byte) (*core.PasswordResetToken, error) {
	row, err := r.q.GetPasswordResetTokenByHash(ctx, tokenHash)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toResetToken(row), nil
}

func (r *PasswordResetRepository) Consume(ctx context.Context, tokenHash []byte) (*core.PasswordResetToken, error) {
	row, err := r.q.ConsumePasswordResetToken(ctx, tokenHash)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toResetToken(row), nil
}

func (r *PasswordResetRepository) Delete(ctx context.Context, tokenHash []byte) error {
	return wrapErr(r.q.DeletePasswordResetToken(ctx, tokenHash))
}

func (r *PasswordResetRepository) DeleteByUser(ctx context.Context, userID uuid.UUID) error {
	return wrapErr(r.q.DeletePasswordResetTokensByUserID(ctx, userID))
}

// VerificationRepository implements core.VerificationRepository.
type VerificationRepository struct {
	q *identitysqlc.Queries
}

var _ core.VerificationRepository = (*VerificationRepository)(nil)

func NewVerificationRepository(db identitysqlc.DBTX) *VerificationRepository {
	return &VerificationRepository{q: identitysqlc.New(db)}
}

func (r *VerificationRepository) Create(ctx context.Context, userID uuid.UUID, email string, tokenHash []byte, expiresAt time.Time) error {
	_, err := r.q.CreateEmailVerificationToken(ctx, identitysqlc.CreateEmailVerificationTokenParams{
		UserID:    userID,
		Email:     email,
		TokenHash: tokenHash,
		ExpiresAt: database.Timestamptz(expiresAt),
	})
	return wrapErr(err)
}

func (r *VerificationRepository) Consume(ctx context.Context, tokenHash []byte) (*core.VerificationToken, error) {
	row, err := r.q.ConsumeEmailVerificationToken(ctx, tokenHash)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &core.VerificationToken{
		ID:        row.ID,
		UserID:    row.UserID,
		Email:     row.Email,
		ExpiresAt: row.ExpiresAt.Time,
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func (r *VerificationRepository) DeleteByUser(ctx context.Context, userID uuid.UUID) error {
	return wrapErr(r.q.DeleteEmailVerificationTokensByUserID(ctx, userID))
}

// TokenPurgeRepository implements core.TokenPurgeRepository. Unlike every
// other identity query these run across all tenants on purpose: retention is
// a maintenance job, and an expired token is unusable in any tenant.
type TokenPurgeRepository struct {
	q *identitysqlc.Queries
}

var _ core.TokenPurgeRepository = (*TokenPurgeRepository)(nil)

func NewTokenPurgeRepository(db identitysqlc.DBTX) *TokenPurgeRepository {
	return &TokenPurgeRepository{q: identitysqlc.New(db)}
}

func (r *TokenPurgeRepository) PurgeExpiredRefreshTokens(ctx context.Context, batchSize int32) (int64, error) {
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return r.q.PurgeExpiredRefreshTokens(ctx, batchSize)
}

func (r *TokenPurgeRepository) PurgeExpiredPasswordResetTokens(ctx context.Context, batchSize int32) (int64, error) {
	return r.q.PurgeExpiredPasswordResetTokens(ctx, batchSize)
}

func (r *TokenPurgeRepository) PurgeExpiredEmailVerificationTokens(ctx context.Context, batchSize int32) (int64, error) {
	return r.q.PurgeExpiredEmailVerificationTokens(ctx, batchSize)
}

func (r *TokenPurgeRepository) PurgeExpiredInvitations(ctx context.Context, batchSize int32) (int64, error) {
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return r.q.PurgeExpiredInvitations(ctx, batchSize)
}

func (r *TokenPurgeRepository) PurgeStaleSessions(ctx context.Context, revokedBefore, idleBefore time.Time, batchSize int32) (int64, error) {
	ctx = database.WithRLSBypass(ctx) // Retention spans every tenant.
	return r.q.PurgeStaleSessions(ctx, identitysqlc.PurgeStaleSessionsParams{
		RevokedBefore: database.Timestamptz(revokedBefore),
		IdleBefore:    database.Timestamptz(idleBefore),
		BatchSize:     batchSize,
	})
}
