package adapter

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// PrincipalReader adapts the identity user repository to
// contracts.PrincipalReader for API-key authentication. It reads the current
// row (tenant-scoped, soft-deleted rows excluded) so bans and role changes are
// reflected on the next request.
type PrincipalReader struct {
	repo repository.UserRepository
}

var _ contracts.PrincipalReader = (*PrincipalReader)(nil)

func NewPrincipalReader(repo repository.UserRepository) *PrincipalReader {
	return &PrincipalReader{repo: repo}
}

func (r *PrincipalReader) GetPrincipal(ctx context.Context, tenantID, userID int64) (contracts.Principal, error) {
	user, err := r.repo.GetByID(tenant.WithID(ctx, tenantID), userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return contracts.Principal{}, apperror.ErrNotFound
		}
		return contracts.Principal{}, err
	}
	return contracts.Principal{
		ID:       user.ID,
		TenantID: user.TenantID,
		Email:    user.Email,
		Name:     user.Name,
		Role:     user.Role,
	}, nil
}
