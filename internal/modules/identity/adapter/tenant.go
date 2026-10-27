package adapter

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantResolver adapts the identity tenant repository to tenant.Resolver.
// Only operational (trialing or active) tenants resolve.
type TenantResolver struct {
	repo core.TenantRepository
}

var _ tenant.Resolver = (*TenantResolver)(nil)

func NewTenantResolver(repo core.TenantRepository) *TenantResolver {
	return &TenantResolver{repo: repo}
}

func (r *TenantResolver) Resolve(ctx context.Context, slug string) (tenant.Tenant, error) {
	found, err := r.repo.GetOperationalBySlug(ctx, slug)
	if err != nil {
		return tenant.Tenant{}, err
	}
	return tenant.Tenant{ID: found.ID, Slug: found.Slug, Name: found.Name}, nil
}
