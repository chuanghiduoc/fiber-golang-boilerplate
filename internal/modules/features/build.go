package features

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	featuresprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/privacy"
	featuresrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/repository"
	featuressvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// Build constructs the feature-flag admin surface. It shares the process
// evaluator so a successful write invalidates the local snapshot immediately,
// and it binds flag writes to one transaction with their Tier 1 audit row.
// Settings are the feature-flag configuration the composition root passes (none today).
type Settings struct{}

// Deps are the contracts features consumes from other modules (none today).
type Deps struct{}

func Build(infra app.Infra, _ Settings, _ Deps) (*Module, error) {
	repo := featuresrepo.New(infra.DB)
	m := New(featureshandler.NewHandler(featuressvc.New(
		repo, infra.Flags, infra.Audit, featuresrepo.NewTransactor(infra.Tx, infra.Audit),
		infra.Catalogs.Features, authz.NewAuthorizer(infra.Catalogs.Permissions))))
	m.privacy = featuresprivacy.New(featuresrepo.NewPrivacyStore(infra.DB))
	m.tenantPurger = featuresrepo.NewTenantPurger(infra.DB)
	return m, nil
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return featurescatalog.Permissions() }
