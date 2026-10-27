package repository

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/google/uuid"
)

// MFARepository implements core.MFARepository. MFA is per global user, so
// no query takes a tenant.
type MFARepository struct {
	q *identitysqlc.Queries
}

var _ core.MFARepository = (*MFARepository)(nil)

// uniqueMFAConfirmedTOTP is the partial unique index that admits one confirmed
// TOTP factor per user.
const uniqueMFAConfirmedTOTP = "idx_mfa_factors_user_confirmed_totp"

func NewMFARepository(db identitysqlc.DBTX) *MFARepository {
	return &MFARepository{q: identitysqlc.New(db)}
}

func toMFAFactor(row identitysqlc.MfaFactor) *core.MFAFactor {
	return &core.MFAFactor{
		ID:               row.ID,
		UserID:           row.UserID,
		Type:             row.Type,
		SecretCiphertext: row.SecretCiphertext,
		ConfirmedAt:      database.TimePtr(row.ConfirmedAt),
		LastUsedStep:     row.LastUsedStep,
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        row.UpdatedAt.Time,
	}
}

func (r *MFARepository) GetTOTP(ctx context.Context, userID uuid.UUID) (*core.MFAFactor, error) {
	row, err := r.q.GetMFAFactor(ctx, userID)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toMFAFactor(row), nil
}

func (r *MFARepository) GetTOTPForUpdate(ctx context.Context, userID uuid.UUID) (*core.MFAFactor, error) {
	row, err := r.q.GetMFAFactorForUpdate(ctx, userID)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toMFAFactor(row), nil
}

func (r *MFARepository) GetConfirmedTOTP(ctx context.Context, userID uuid.UUID) (*core.MFAFactor, error) {
	row, err := r.q.GetConfirmedMFAFactor(ctx, userID)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toMFAFactor(row), nil
}

func (r *MFARepository) UpsertUnconfirmedTOTP(ctx context.Context, userID uuid.UUID, secretCiphertext string) (*core.MFAFactor, error) {
	row, err := r.q.UpsertUnconfirmedMFAFactor(ctx, identitysqlc.UpsertUnconfirmedMFAFactorParams{
		UserID:           userID,
		SecretCiphertext: secretCiphertext,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toMFAFactor(row), nil
}

func (r *MFARepository) ConfirmTOTP(ctx context.Context, id, userID uuid.UUID, step int64) (bool, error) {
	n, err := r.q.ConfirmMFAFactor(ctx, identitysqlc.ConfirmMFAFactorParams{
		ID: id, UserID: userID, Step: step,
	})
	if err != nil {
		// A concurrent enrollment confirmed first; the partial unique index
		// keeps one confirmed factor, and the caller reports "already enabled".
		if name, ok := uniqueConstraint(err); ok && name == uniqueMFAConfirmedTOTP {
			return false, nil
		}
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *MFARepository) ConsumeTOTPStep(ctx context.Context, userID uuid.UUID, step int64) (bool, error) {
	n, err := r.q.ConsumeMFAStep(ctx, identitysqlc.ConsumeMFAStepParams{UserID: userID, Step: step})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *MFARepository) DeleteTOTP(ctx context.Context, userID uuid.UUID) error {
	return wrapErr(r.q.DeleteMFAFactor(ctx, userID))
}

func (r *MFARepository) DeletePendingTOTP(ctx context.Context, userID uuid.UUID) error {
	return wrapErr(r.q.DeletePendingMFAFactor(ctx, userID))
}

func (r *MFARepository) DeleteRecoveryCodes(ctx context.Context, userID uuid.UUID) error {
	return wrapErr(r.q.DeleteRecoveryCodes(ctx, userID))
}

func (r *MFARepository) CreateRecoveryCodes(ctx context.Context, userID uuid.UUID, hashes [][]byte) error {
	return wrapErr(r.q.CreateRecoveryCodes(ctx, identitysqlc.CreateRecoveryCodesParams{
		UserID: userID, CodeHashes: hashes,
	}))
}

func (r *MFARepository) UseRecoveryCode(ctx context.Context, userID uuid.UUID, hash []byte) (bool, error) {
	n, err := r.q.UseRecoveryCode(ctx, identitysqlc.UseRecoveryCodeParams{UserID: userID, CodeHash: hash})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *MFARepository) CountUnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) (int64, error) {
	n, err := r.q.CountUnusedRecoveryCodes(ctx, userID)
	if err != nil {
		return 0, wrapErr(err)
	}
	return n, nil
}
