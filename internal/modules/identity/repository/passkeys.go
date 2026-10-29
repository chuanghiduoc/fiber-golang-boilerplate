package repository

import (
	"context"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/google/uuid"
)

// uniquePasskeyCredentialID is the unique index that makes a WebAuthn
// credential id globally single-use.
const uniquePasskeyCredentialID = "uq_webauthn_credentials_credential_id"

// PasskeyRepository implements core.PasskeyRepository. Passkeys are per
// global user, so no query takes a tenant.
type PasskeyRepository struct {
	q *identitysqlc.Queries
}

var _ core.PasskeyRepository = (*PasskeyRepository)(nil)

func NewPasskeyRepository(db identitysqlc.DBTX) *PasskeyRepository {
	return &PasskeyRepository{q: identitysqlc.New(db)}
}

func toPasskey(row identitysqlc.WebauthnCredential) *core.Passkey {
	return &core.Passkey{
		ID:             row.ID,
		UserID:         row.UserID,
		CredentialID:   row.CredentialID,
		PublicKey:      row.PublicKey,
		SignCount:      row.SignCount,
		AAGUID:         row.Aaguid,
		Transports:     row.Transports,
		BackupEligible: row.BackupEligible,
		BackupState:    row.BackupState,
		Name:           row.Name,
		CreatedAt:      row.CreatedAt.Time,
		LastUsedAt:     database.TimePtr(row.LastUsedAt),
		UpdatedAt:      row.UpdatedAt.Time,
	}
}

func (r *PasskeyRepository) Create(ctx context.Context, passkey core.Passkey) (*core.Passkey, error) {
	row, err := r.q.CreatePasskey(ctx, identitysqlc.CreatePasskeyParams{
		UserID:         passkey.UserID,
		CredentialID:   passkey.CredentialID,
		PublicKey:      passkey.PublicKey,
		SignCount:      passkey.SignCount,
		Aaguid:         passkey.AAGUID,
		Transports:     passkey.Transports,
		BackupEligible: passkey.BackupEligible,
		BackupState:    passkey.BackupState,
		Name:           passkey.Name,
	})
	if err != nil {
		if name, ok := uniqueConstraint(err); ok && name == uniquePasskeyCredentialID {
			return nil, core.ErrPasskeyAlreadyRegistered
		}
		return nil, wrapErr(err)
	}
	return toPasskey(row), nil
}

func (r *PasskeyRepository) ListByUser(ctx context.Context, userID uuid.UUID) ([]core.Passkey, error) {
	rows, err := r.q.ListPasskeysByUser(ctx, userID)
	if err != nil {
		return nil, wrapErr(err)
	}
	out := make([]core.Passkey, len(rows))
	for i, row := range rows {
		out[i] = *toPasskey(row)
	}
	return out, nil
}

func (r *PasskeyRepository) ListByUserLimited(ctx context.Context, userID uuid.UUID, limit int32) ([]core.Passkey, bool, error) {
	if limit <= 0 {
		return []core.Passkey{}, false, nil
	}
	rows, err := r.q.ListPasskeysByUserLimited(ctx, identitysqlc.ListPasskeysByUserLimitedParams{
		UserID: userID, RowLimit: limit + 1,
	})
	if err != nil {
		return nil, false, wrapErr(err)
	}
	truncated := len(rows) > int(limit)
	if truncated {
		rows = rows[:limit]
	}
	out := make([]core.Passkey, len(rows))
	for i, row := range rows {
		out[i] = *toPasskey(row)
	}
	return out, truncated, nil
}

func (r *PasskeyRepository) GetByCredentialID(ctx context.Context, credentialID []byte) (*core.Passkey, error) {
	row, err := r.q.GetPasskeyByCredentialID(ctx, credentialID)
	if err != nil {
		return nil, wrapErr(err)
	}
	return toPasskey(row), nil
}

func (r *PasskeyRepository) Get(ctx context.Context, userID, id uuid.UUID) (*core.Passkey, error) {
	row, err := r.q.GetPasskey(ctx, identitysqlc.GetPasskeyParams{UserID: userID, ID: id})
	if err != nil {
		return nil, wrapErr(err)
	}
	return toPasskey(row), nil
}

func (r *PasskeyRepository) Delete(ctx context.Context, userID, id uuid.UUID) (bool, error) {
	n, err := r.q.DeletePasskey(ctx, identitysqlc.DeletePasskeyParams{UserID: userID, ID: id})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *PasskeyRepository) DeleteByUser(ctx context.Context, userID uuid.UUID) error {
	return wrapErr(r.q.DeletePasskeysByUser(ctx, userID))
}

func (r *PasskeyRepository) UpdateAfterAssertion(ctx context.Context, userID, id uuid.UUID, signCount int64, backupState bool) (bool, error) {
	n, err := r.q.UpdatePasskeyAfterAssertion(ctx, identitysqlc.UpdatePasskeyAfterAssertionParams{
		SignCount:   signCount,
		BackupState: backupState,
		ID:          id,
		UserID:      userID,
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}
