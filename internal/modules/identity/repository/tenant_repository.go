package repository

import (
	"context"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

// TenantRepository exposes tenant lookups without leaking SQL details into the
// adapter layer.
type TenantRepository interface {
	ResolveBySlug(ctx context.Context, slug string) (identitysqlc.Tenant, error)
	GetByID(ctx context.Context, id int64) (identitysqlc.Tenant, error)
}

type tenantRepository struct {
	q *identitysqlc.Queries
}

func NewTenantRepository(db identitysqlc.DBTX) TenantRepository {
	return &tenantRepository{q: identitysqlc.New(db)}
}

func (r *tenantRepository) ResolveBySlug(ctx context.Context, slug string) (identitysqlc.Tenant, error) {
	tenant, err := r.q.GetTenantBySlug(ctx, slug)
	if err != nil {
		return identitysqlc.Tenant{}, wrapErr(err)
	}
	return tenant, nil
}

func (r *tenantRepository) GetByID(ctx context.Context, id int64) (identitysqlc.Tenant, error) {
	tenant, err := r.q.GetTenantByID(ctx, id)
	if err != nil {
		return identitysqlc.Tenant{}, wrapErr(err)
	}
	return tenant, nil
}
