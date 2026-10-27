// Package catalog declares the files module's permissions (permissions.go),
// error codes (errors.go), feature flags (flags.go), and i18n strings
// (locales/). It is a leaf package so the module root, handlers, and services
// can reference the constants without an import cycle; the module root
// registers them through the app provider hooks.
//
// Values are part of the public API and must never change once released.
package catalog

import (
	"embed"
	"io/fs"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "files"

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
