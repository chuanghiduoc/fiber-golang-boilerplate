package repository

import (
	"context"
	"errors"
	"testing"

	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

type fakeTenantRepo struct {
	byID   map[int64]identitysqlc.Tenant
	bySlug map[string]identitysqlc.Tenant
	err    error
}

func (f *fakeTenantRepo) ResolveBySlug(ctx context.Context, slug string) (identitysqlc.Tenant, error) {
	t, ok := f.bySlug[slug]
	if !ok {
		return identitysqlc.Tenant{}, apperror.ErrNotFound
	}
	if t.Status != "active" {
		return identitysqlc.Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

func (f *fakeTenantRepo) GetByID(ctx context.Context, id int64) (identitysqlc.Tenant, error) {
	t, ok := f.byID[id]
	if !ok || t.Status != "active" {
		return identitysqlc.Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

func (f *fakeTenantRepo) GetBySlugIncludingSuspended(ctx context.Context, slug string) (identitysqlc.Tenant, error) {
	if f.err != nil {
		return identitysqlc.Tenant{}, f.err
	}
	t, ok := f.bySlug[slug]
	if !ok {
		return identitysqlc.Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

func (f *fakeTenantRepo) GetByIDIncludingSuspended(ctx context.Context, id int64) (identitysqlc.Tenant, error) {
	if f.err != nil {
		return identitysqlc.Tenant{}, f.err
	}
	t, ok := f.byID[id]
	if !ok {
		return identitysqlc.Tenant{}, apperror.ErrNotFound
	}
	return t, nil
}

func TestStartupTenants_AllowsSuspendedDefault(t *testing.T) {
	suspended := identitysqlc.Tenant{ID: 1, Slug: "default", Name: "Default", Status: "suspended"}
	repo := &fakeTenantRepo{
		byID:   map[int64]identitysqlc.Tenant{1: suspended},
		bySlug: map[string]identitysqlc.Tenant{"default": suspended},
	}

	// Request resolution stays active-only.
	if _, err := repo.ResolveBySlug(context.Background(), "default"); !errors.Is(err, apperror.ErrNotFound) {
		t.Fatalf("active resolver must hide a suspended tenant, got %v", err)
	}

	baseline, configured, err := StartupTenants(context.Background(), repo, "default")
	if err != nil {
		t.Fatalf("startup must accept a suspended default tenant: %v", err)
	}
	if baseline.ID != 1 || configured.ID != 1 {
		t.Fatalf("startup tenants = (%d, %d), want (1, 1)", baseline.ID, configured.ID)
	}
	if configured.Status != "suspended" {
		t.Fatalf("configured status = %q, want suspended", configured.Status)
	}
}

func TestStartupTenants_MissingTenantsFail(t *testing.T) {
	repo := &fakeTenantRepo{
		byID:   map[int64]identitysqlc.Tenant{},
		bySlug: map[string]identitysqlc.Tenant{},
	}
	if _, _, err := StartupTenants(context.Background(), repo, "default"); err == nil {
		t.Fatal("startup must fail when the baseline tenant is missing")
	}

	repo.byID[1] = identitysqlc.Tenant{ID: 1, Slug: "default", Status: "active"}
	if _, _, err := StartupTenants(context.Background(), repo, "acme"); err == nil {
		t.Fatal("startup must fail when the configured default tenant is missing")
	}
}
