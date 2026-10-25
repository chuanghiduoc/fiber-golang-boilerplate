package repository

import (
	"context"
	"time"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
)

// uniqueTenantsSlug is PostgreSQL's name for the inline slug UNIQUE.
const uniqueTenantsSlug = "tenants_slug_key"

// TenantRepository implements service.TenantRepository.
type TenantRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.TenantRepository = (*TenantRepository)(nil)

func NewTenantRepository(db identitysqlc.DBTX) *TenantRepository {
	return &TenantRepository{q: identitysqlc.New(db)}
}

func toTenant(row identitysqlc.Tenant) *identityservice.Tenant {
	return &identityservice.Tenant{
		ID:                  row.ID,
		Slug:                row.Slug,
		Name:                row.Name,
		Status:              identityservice.TenantStatus(row.Status),
		StatusChangedAt:     row.StatusChangedAt.Time,
		TrialEndsAt:         timePtr(row.TrialEndsAt),
		DeletionScheduledAt: timePtr(row.DeletionScheduledAt),
		CreatedAt:           row.CreatedAt.Time,
		UpdatedAt:           row.UpdatedAt.Time,
	}
}

func oneTenant(row identitysqlc.Tenant, err error) (*identityservice.Tenant, error) {
	if err != nil {
		return nil, wrapErr(err)
	}
	return toTenant(row), nil
}

func (r *TenantRepository) GetOperationalBySlug(ctx context.Context, slug string) (*identityservice.Tenant, error) {
	return oneTenant(r.q.GetOperationalTenantBySlug(ctx, slug))
}

func (r *TenantRepository) GetOperationalByID(ctx context.Context, id int64) (*identityservice.Tenant, error) {
	return oneTenant(r.q.GetOperationalTenantByID(ctx, id))
}

func (r *TenantRepository) GetByID(ctx context.Context, id int64) (*identityservice.Tenant, error) {
	return oneTenant(r.q.GetTenantByID(ctx, id))
}

func (r *TenantRepository) GetBySlug(ctx context.Context, slug string) (*identityservice.Tenant, error) {
	return oneTenant(r.q.GetTenantBySlug(ctx, slug))
}

func (r *TenantRepository) GetByIDForUpdate(ctx context.Context, id int64) (*identityservice.Tenant, error) {
	return oneTenant(r.q.GetTenantByIDForUpdate(ctx, id))
}

// Create inserts the tenant and its settings row. It needs a transaction
// for the two inserts to be atomic, so callers use it through Transactor.
func (r *TenantRepository) Create(ctx context.Context, t identityservice.NewTenant) (*identityservice.Tenant, error) {
	row, err := r.q.CreateTenant(ctx, identitysqlc.CreateTenantParams{
		Slug:        t.Slug,
		Name:        t.Name,
		Status:      string(t.Status),
		TrialEndsAt: optionalTimestamptz(t.TrialEndsAt),
	})
	if err != nil {
		if name, ok := uniqueConstraint(err); ok && name == uniqueTenantsSlug {
			return nil, identityservice.ErrTenantSlugTaken
		}
		return nil, wrapErr(err)
	}
	if err := r.q.CreateTenantSettings(ctx, identitysqlc.CreateTenantSettingsParams{
		TenantID: row.ID,
		Locale:   t.Locale,
		Timezone: t.Timezone,
	}); err != nil {
		return nil, wrapErr(err)
	}
	return toTenant(row), nil
}

func (r *TenantRepository) UpdateStatus(ctx context.Context, id int64, status identityservice.TenantStatus, deletionScheduledAt *time.Time) (*identityservice.Tenant, error) {
	return oneTenant(r.q.UpdateTenantStatus(ctx, identitysqlc.UpdateTenantStatusParams{
		ID:                  id,
		Status:              string(status),
		DeletionScheduledAt: optionalTimestamptz(deletionScheduledAt),
	}))
}

func (r *TenantRepository) Settings(ctx context.Context, tenantID int64) (*identityservice.TenantSettings, error) {
	row, err := r.q.GetTenantSettings(ctx, tenantID)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &identityservice.TenantSettings{TenantID: row.TenantID, Locale: row.Locale, Timezone: row.Timezone}, nil
}
