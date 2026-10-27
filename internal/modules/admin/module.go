// Package admin is the back-office facade. Every former /admin route now lives
// with the module that owns its data: /members, /roles, /invitations, and
// /stats are identity operations, /files is the files module, and self-service
// API keys are apikeys operations. The canonical route map therefore assigns
// this module no HTTP operations, and its handler, DTOs, and service are gone.
//
// The module stays registered so the composition root keeps an explicit record
// of the retired facade and the route-less shape is covered by test/arch.
package admin

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
)

// Module owns no routes. It exists only so the retired back-office facade has a
// named, reviewed place in the module registry.
type Module struct{}

// New returns the route-less facade module.
func New() *Module { return &Module{} }

// Name identifies the module in the registry.
func (m *Module) Name() string { return "admin" }

// RegisterRoutes implements app.RouteProvider. The route map assigns this
// module no operations; test/arch requires every module to declare its routes,
// even an empty set.
func (m *Module) RegisterRoutes(*httpapi.API, app.Deps) {}
