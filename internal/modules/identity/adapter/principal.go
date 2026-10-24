package adapter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// PrincipalReader adapts the identity user repository to
// identitycontracts.PrincipalReader for API-key authentication. It reads the current
// row (tenant-scoped, soft-deleted rows excluded) so bans and role changes are
// reflected on the next request.
type PrincipalReader struct {
	repo repository.UserRepository
}

var _ identitycontracts.PrincipalReader = (*PrincipalReader)(nil)

func NewPrincipalReader(repo repository.UserRepository) *PrincipalReader {
	return &PrincipalReader{repo: repo}
}

func (r *PrincipalReader) GetPrincipal(ctx context.Context, tenantID, userID int64) (identitycontracts.Principal, error) {
	user, err := r.repo.GetPrincipal(tenant.WithID(ctx, tenantID), userID)
	return toPrincipal(user, err)
}

// LockPrincipalTx loads the active principal inside the caller's transaction
// and holds its user row FOR SHARE until that transaction ends, so a
// concurrent ban or delete cannot commit while the caller writes a credential
// for this owner. It returns apperror.ErrNotFound when the account is not
// active. The method takes a pgx.Tx, so it cannot be part of the stdlib-only
// contracts package; the apikeys service discovers it by type assertion.
func (r *PrincipalReader) LockPrincipalTx(ctx context.Context, tx pgx.Tx, tenantID, userID int64) (identitycontracts.Principal, error) {
	user, err := repository.NewUserRepository(tx).LockPrincipal(tenant.WithID(ctx, tenantID), userID)
	return toPrincipal(user, err)
}

func toPrincipal(user *identitysqlc.GetUserPrincipalRow, err error) (identitycontracts.Principal, error) {
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return identitycontracts.Principal{}, apperror.ErrNotFound
		}
		return identitycontracts.Principal{}, err
	}
	principal := identitycontracts.Principal{
		ID:           user.ID,
		TenantID:     user.TenantID,
		Email:        user.Email,
		Name:         user.Name,
		Role:         user.Role,
		TenantStatus: user.TenantStatus,
	}
	if user.CredentialsRevokedAt.Valid {
		revokedAt := user.CredentialsRevokedAt.Time
		principal.CredentialsRevokedAt = &revokedAt
	}
	return principal, nil
}
