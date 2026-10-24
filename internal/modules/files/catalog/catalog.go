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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Owner is the module name (Module.Name()) that owns these entries.
const Owner = "files"

// Register adds this catalog to the process-wide authz, apperror, and
// features catalogs. At startup app.RegisterCatalogs does the same through
// the module provider hooks; unit tests of the module's handlers and services
// call it directly because they do not build the app. Repeating it is a no-op.
func Register() error {
	if err := authz.Register(Owner, Permissions()...); err != nil {
		return err
	}
	if err := apperror.Register(Owner, ErrorCodes()); err != nil {
		return err
	}
	return features.Register(Owner, FeatureFlags()...)
}

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
