package handler

import (
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// testErrors is the instance catalog built from identity's providers, exactly
// like the composition root.
var testErrors = newTestErrors()

func newTestErrors() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: identitycatalog.Owner, Specs: identitycatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

// resolvedStatus renders the error's code through the test catalog, the same
// way the HTTP layer does.
func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := testErrors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
