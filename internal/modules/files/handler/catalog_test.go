package handler

import filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"

// The app registers module catalogs at startup; these handler tests run
// without the app, so they register the files codes, flags, and permissions.
func init() {
	if err := filescatalog.Register(); err != nil {
		panic(err)
	}
}
