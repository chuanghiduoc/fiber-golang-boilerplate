package fakes

import (
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Catalog and Errors are instance catalogs built from identity's providers,
// exactly like the composition root. Package tests never mutate process-wide
// state.
var (
	Catalog = newCatalog()
	Errors  = newErrors()
)

func newCatalog() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: identitycatalog.Owner, Grants: identitycatalog.Permissions()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func newErrors() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: identitycatalog.Owner, Specs: identitycatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}
