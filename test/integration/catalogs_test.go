//go:build integration

package integration_test

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/testutil"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// integrationCatalogs is the composition-root catalog set (every module's
// errors, permissions, and feature flags) for harnesses that wire modules by
// hand.
func integrationCatalogs() app.Catalogs { return testutil.Catalogs() }

// resolvedStatus is the HTTP status the composition-root catalog renders for
// appErr; constructors no longer stamp it, the catalog resolves it.
func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := integrationCatalogs().Errors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
