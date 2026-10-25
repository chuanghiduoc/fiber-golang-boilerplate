package adapter

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	identityrepository "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// PrincipalReader adapts identity memberships to
// identitycontracts.PrincipalReader for API-key authentication. An API key's
// owner is a membership: it reads the current active membership (tenant
// scoped) so bans, removals, and role changes are reflected on the next
// request.
type PrincipalReader struct {
	memberships identityservice.MembershipRepository
}

var _ identitycontracts.PrincipalReader = (*PrincipalReader)(nil)

func NewPrincipalReader(memberships identityservice.MembershipRepository) *PrincipalReader {
	return &PrincipalReader{memberships: memberships}
}

func (r *PrincipalReader) GetPrincipal(ctx context.Context, tenantID, userID int64) (identitycontracts.Principal, error) {
	principal, err := r.memberships.GetPrincipal(tenant.WithID(ctx, tenantID), userID)
	return toPrincipal(principal, err)
}

// LockPrincipalTx loads the active principal inside the caller's transaction
// and holds its membership row FOR SHARE until that transaction ends, so a
// concurrent ban or removal cannot commit while the caller writes a
// credential for this owner. It returns apperror.ErrNotFound when the
// membership is not active. The method takes a pgx.Tx, so it cannot be part
// of the stdlib-only contracts package; the apikeys service discovers it by
// type assertion.
func (r *PrincipalReader) LockPrincipalTx(ctx context.Context, tx pgx.Tx, tenantID, userID int64) (identitycontracts.Principal, error) {
	principal, err := identityrepository.NewMembershipRepository(tx).LockPrincipal(tenant.WithID(ctx, tenantID), userID)
	return toPrincipal(principal, err)
}

func toPrincipal(p *identityservice.Principal, err error) (identitycontracts.Principal, error) {
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return identitycontracts.Principal{}, apperror.ErrNotFound
		}
		return identitycontracts.Principal{}, err
	}
	status := string(p.TenantStatus)
	if p.TenantStatus.Operational() {
		// The contract's single serving state: a trialing tenant serves
		// requests exactly like an active one.
		status = identitycontracts.TenantStatusActive
	}
	return identitycontracts.Principal{
		ID:                   p.UserID,
		TenantID:             p.TenantID,
		Email:                p.Email,
		Name:                 p.Name,
		Role:                 p.Role,
		Permissions:          p.Permissions,
		Platform:             p.Platform,
		TenantStatus:         status,
		CredentialsRevokedAt: p.CredentialsRevokedAt,
	}, nil
}
