// Package features owns feature-flag administration routes. Evaluation and
// the compile-time catalog live in internal/platform/features; this module is
// the platform and tenant back-office surface for database defaults, kill
// switches, and targets.
package features

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	privacycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/privacy"
	tenancycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/tenancy"
	featurescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/catalog"
	featureshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/handler"
	featuresprivacy "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/privacy"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
)

// Module wires the feature-flag HTTP surface into the application.
type Module struct {
	handler      *featureshandler.Handler
	privacy      *featuresprivacy.Provider
	tenantPurger tenancycontracts.Purger
}

var (
	_ app.RouteProvider   = (*Module)(nil)
	_ app.PrivacyProvider = (*Module)(nil)
)

// New builds the module from its handler.
func New(handler *featureshandler.Handler) *Module {
	return &Module{handler: handler}
}

// Name identifies the module in the registry.
func (m *Module) Name() string { return featurescatalog.Owner }

// RegisterPrivacy implements app.PrivacyProvider.
func (m *Module) RegisterPrivacy(registry *privacycontracts.Registry) {
	if m.privacy == nil {
		return
	}
	registry.Register(privacycontracts.Provider{Name: "features", Exporter: m.privacy, Eraser: m.privacy})
}

// RegisterRoutes implements app.RouteProvider.
func (m *Module) RegisterRoutes(api *httpapi.API, _ app.Deps) {
	m.handler.Register(api)
}
