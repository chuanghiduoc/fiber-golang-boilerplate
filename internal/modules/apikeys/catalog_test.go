package apikeys

import apikeyscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/catalog"

// The route tests mount the module on a bare Fiber app (no app.Register), so
// they register the module's permissions themselves.
func init() {
	if err := apikeyscatalog.Register(); err != nil {
		panic(err)
	}
}
