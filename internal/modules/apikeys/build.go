package apikeys

import (
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	apikeyadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/adapter"
	apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"
	apikeyhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/handler"
	apikeysprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/privacy"
	apikeyrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/repository"
	apikeyservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
)

// Provides lists what apikeys supplies to the platform.
type Provides struct {
	// Verifier authenticates `Authorization: ApiKey` on every module's routes
	// (app.Deps.APIKeyVerifier).
	Verifier *apikey.Verifier
}

// Build constructs the API-key lifecycle surface and the request verifier.
// The verifier only needs a hash lookup plus the identity principal contract.
func Build(infra app.Infra, principals identitycontracts.PrincipalReader, ranks identitycontracts.MemberRankChecker, settings Settings) (*Module, Provides) {
	repo := apikeyrepo.NewAPIKeyRepository(infra.DB)
	verifier := apikey.NewVerifier(apikeyadapter.NewVerifierStore(repo), principals)
	svc := apikeyservice.NewService(repo, principals, ranks, apikeyrepo.NewTransactor(infra.Tx, infra.Audit, principals), settings.MaxLifetime)
	m := New(apikeyhandler.NewHandler(svc))
	m.privacy = apikeysprivacy.New(apikeyrepo.NewPrivacyStore(infra.DB))
	m.tenantPurger = apikeyrepo.NewTenantPurger(infra.DB)
	return m, Provides{Verifier: verifier}
}

// Settings are the API-key configuration the composition root passes in.
type Settings struct {
	// MaxLifetime caps newly issued credentials (APIKEYS_MAX_LIFETIME).
	MaxLifetime time.Duration
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return apikeyscatalog.Permissions() }
