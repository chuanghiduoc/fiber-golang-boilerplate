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
// evaluator so a successful write invalidates the local snapshot immediately.
func Build(infra app.Infra) *Module {
	repo := featuresrepo.New(infra.DB)
	m := New(featureshandler.NewHandler(featuressvc.New(repo, infra.Flags, infra.Audit)))
	m.privacy = featuresprivacy.New(featuresrepo.NewPrivacyStore(infra.DB))
	m.tenantPurger = featuresrepo.NewTenantPurger(infra.DB)
	return m
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return featurescatalog.Permissions() }
