package apikeys

import (
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
func Build(infra app.Infra, principals identitycontracts.PrincipalReader) (*Module, Provides) {
	repo := apikeyrepo.NewAPIKeyRepository(infra.Pool)
	verifier := apikey.NewVerifier(apikeyadapter.NewVerifierStore(repo), principals)
	svc := apikeyservice.NewService(repo, principals, infra.Tx, infra.Audit)
	m := New(apikeyhandler.NewHandler(svc))
	m.privacy = apikeysprivacy.New(apikeyrepo.NewPrivacyStore(infra.Pool))
	return m, Provides{Verifier: verifier}
}

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return apikeyscatalog.Permissions() }
