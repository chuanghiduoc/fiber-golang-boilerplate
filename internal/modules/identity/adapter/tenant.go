package adapter

import (
	"context"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantResolver adapts the identity tenant repository to tenant.Resolver.
// Only operational (trialing or active) tenants resolve.
type TenantResolver struct {
	repo identityservice.TenantRepository
}

var _ tenant.Resolver = (*TenantResolver)(nil)

func NewTenantResolver(repo identityservice.TenantRepository) *TenantResolver {
	return &TenantResolver{repo: repo}
}

func (r *TenantResolver) Resolve(ctx context.Context, slug string) (tenant.Tenant, error) {
	found, err := r.repo.GetOperationalBySlug(ctx, slug)
	if err != nil {
		return tenant.Tenant{}, err
	}
	return tenant.Tenant{ID: found.ID, Slug: found.Slug, Name: found.Name}, nil
}
