package arch_test

import (
	"testing"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/i18n"
)

// TestEveryRegisteredCodeHasLocalizedTitles builds the production module
// set, registers its catalogs, merges its locales exactly as cmd/api does,
// and requires an en and vi title for every error code (platform and
// module). i18n.New itself enforces key/placeholder parity and collisions.
func TestEveryRegisteredCodeHasLocalizedTitles(t *testing.T) {
	modules, _ := productionModules(t)
	catalogs, err := app.BuildCatalogs(modules...)
	if err != nil {
		t.Fatalf("build catalogs: %v", err)
	}
	bundle, err := i18n.New("en", app.LocaleSources(modules...)...)
	if err != nil {
		t.Fatalf("merged i18n bundle: %v", err)
	}
	for _, code := range catalogs.Errors.Codes() {
		key := "errors." + string(code) + ".title"
		// i18n.New already guarantees every locale has the same keys, so a
		// key present in one locale is present in all of them.
		for _, locale := range bundle.SupportedLocales() {
			if !bundle.Has(locale, key) {
				t.Errorf("%s: missing %q (add it to the owning module's locales)", locale, key)
			}
		}
	}
}
