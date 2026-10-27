package admin

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
)

// Settings are the admin facade's configuration (none today).
type Settings struct{}

// Deps are the contracts the admin facade consumes (none: its contract support
// is composed by the calling modules).
type Deps struct{}

// Build constructs the route-less back-office facade. The contract support it
// used to compose (/members, /roles, /stats from identity; /files from files)
// is served by those modules directly.
func Build(_ app.Infra, _ Settings, _ Deps) (*Module, error) { return New(), nil }

var (
	_ app.Module        = (*Module)(nil)
	_ app.RouteProvider = (*Module)(nil)
)
