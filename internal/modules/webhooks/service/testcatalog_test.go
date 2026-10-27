package service

import (
	webhookscatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// testErrors is the instance catalog built from the webhooks providers.
var testErrors = newTestErrors()

func newTestErrors() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: webhookscatalog.Owner, Specs: webhookscatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := testErrors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
