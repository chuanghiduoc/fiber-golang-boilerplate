package handler

import identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"

// The app registers module catalogs at startup; these handler tests run
// without the app, so they register identity's codes, flags, and permissions.
func init() {
	if err := identitycatalog.Register(); err != nil {
		panic(err)
	}
}
