//go:build integration

package service_test

import (
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Instance catalogs built from identity's providers, exactly like the
// composition root. Integration tests never touch process-wide state.
var (
	testCatalog = newTestAuthzCatalog()
	testErrors  = newTestErrorCatalog()
)

// newTestAuthzCatalog is the full composition-root permission catalog: the
// fixture synchronizes every module's grants into the database, so subjects
// built here hold exactly what the stored system roles hold.
func newTestAuthzCatalog() *authz.Catalog { return testutil.Catalogs().Permissions }

func newTestErrorCatalog() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: identitycatalog.Owner, Specs: identitycatalog.Errors()}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}

func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := testErrors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
