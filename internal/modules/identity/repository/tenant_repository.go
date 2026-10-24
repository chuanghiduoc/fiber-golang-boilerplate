package repository

import (
	"context"
	"fmt"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
)

// TenantRepository exposes tenant lookups without leaking SQL details into the
// adapter layer. ResolveBySlug/GetByID return active tenants only; request
// handling must never operate on a suspended tenant. Startup checks use the
// *IncludingSuspended variants so existence is evaluated separately from
// request eligibility.
type TenantRepository interface {
	ResolveBySlug(ctx context.Context, slug string) (identitysqlc.Tenant, error)
	GetByID(ctx context.Context, id int64) (identitysqlc.Tenant, error)
	GetBySlugIncludingSuspended(ctx context.Context, slug string) (identitysqlc.Tenant, error)
	GetByIDIncludingSuspended(ctx context.Context, id int64) (identitysqlc.Tenant, error)
}

type tenantRepository struct {
	q *identitysqlc.Queries
}

func NewTenantRepository(db identitysqlc.DBTX) TenantRepository {
	return &tenantRepository{q: identitysqlc.New(db)}
}

func (r *tenantRepository) ResolveBySlug(ctx context.Context, slug string) (identitysqlc.Tenant, error) {
	found, err := r.q.GetTenantBySlug(ctx, slug)
	if err != nil {
		return identitysqlc.Tenant{}, wrapErr(err)
	}
	return found, nil
}

func (r *tenantRepository) GetByID(ctx context.Context, id int64) (identitysqlc.Tenant, error) {
	found, err := r.q.GetTenantByID(ctx, id)
	if err != nil {
		return identitysqlc.Tenant{}, wrapErr(err)
	}
	return found, nil
}

func (r *tenantRepository) GetBySlugIncludingSuspended(ctx context.Context, slug string) (identitysqlc.Tenant, error) {
	found, err := r.q.GetTenantBySlugIncludingSuspended(ctx, slug)
	if err != nil {
		return identitysqlc.Tenant{}, wrapErr(err)
	}
	return found, nil
}

func (r *tenantRepository) GetByIDIncludingSuspended(ctx context.Context, id int64) (identitysqlc.Tenant, error) {
	found, err := r.q.GetTenantByIDIncludingSuspended(ctx, id)
	if err != nil {
		return identitysqlc.Tenant{}, wrapErr(err)
	}
	return found, nil
}

// StartupTenants verifies the baseline compatibility tenant (id 1) and the
// configured default tenant exist, ignoring status. A suspended tenant is a
// valid startup state: request resolution will return 404 until it is
// reactivated, but the process and seed command must still start.
func StartupTenants(ctx context.Context, repo TenantRepository, defaultSlug string) (baseline, configured identitysqlc.Tenant, err error) {
	baseline, err = repo.GetByIDIncludingSuspended(ctx, tenant.DefaultID)
	if err != nil {
		return identitysqlc.Tenant{}, identitysqlc.Tenant{},
			fmt.Errorf("baseline tenant (id %d) is missing; run cmd/migrate: %w", tenant.DefaultID, err)
	}
	configured, err = repo.GetBySlugIncludingSuspended(ctx, defaultSlug)
	if err != nil {
		return identitysqlc.Tenant{}, identitysqlc.Tenant{},
			fmt.Errorf("configured default tenant %q is missing; run cmd/migrate or fix TENANT_DEFAULT_SLUG: %w", defaultSlug, err)
	}
	return baseline, configured, nil
}
