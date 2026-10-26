package catalog

import (
	"embed"
	"io/fs"
)

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
