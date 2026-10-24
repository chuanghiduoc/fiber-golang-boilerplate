package adapter

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantResolver adapts the identity tenant repository to tenant.Resolver.
type TenantResolver struct {
	repo repository.TenantRepository
}

var _ tenant.Resolver = (*TenantResolver)(nil)

func NewTenantResolver(repo repository.TenantRepository) *TenantResolver {
	return &TenantResolver{repo: repo}
}

func (r *TenantResolver) Resolve(ctx context.Context, slug string) (tenant.Tenant, error) {
	found, err := r.repo.ResolveBySlug(ctx, slug)
	if err != nil {
		return tenant.Tenant{}, err
	}
	return tenant.Tenant{ID: found.ID, Slug: found.Slug, Name: found.Name}, nil
}
