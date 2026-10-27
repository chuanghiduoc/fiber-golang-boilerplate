package catalog

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Test helpers build instance catalogs from this module's providers exactly
// like the composition root does, instead of mutating process-wide state.

func testErrors() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: Owner, Specs: Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func testPermissions() *authz.Catalog {
	catalog := authz.NewCatalog()
	if err := catalog.Add(authz.Provider{Owner: Owner, Grants: Permissions()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func testFlags() *features.Catalog {
	catalog := features.NewCatalog()
	if err := catalog.Add(features.Provider{Owner: Owner, Definitions: FeatureFlags()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}
