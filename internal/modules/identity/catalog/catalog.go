// Package catalog declares the identity module's permissions (permissions.go),
// error codes (errors.go), feature flags (flags.go), and i18n strings
// (locales/). It is a leaf package (platform imports only) so the module
// root, handlers, and services can all reference the constants without an
// import cycle; the module root registers them through the app provider
// hooks (PermissionProvider, ErrorProvider, FlagProvider, LocaleProvider).
//
// Values are part of the public API (error codes, API-key scopes, flag keys)
// and must never change once released.
package catalog

import (
	"embed"
	"io/fs"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "identity"

//go:embed locales/*.json
var localesFS embed.FS

// Locales returns en.json/vi.json for app.LocaleProvider.
func Locales() fs.FS {
	sub, err := fs.Sub(localesFS, "locales")
	if err != nil {
		// fs.Sub only fails for an invalid path literal: a build bug.
		panic(err)
	}
	return sub
}
