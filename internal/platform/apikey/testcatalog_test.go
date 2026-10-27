package apikey

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// testScopeRead is a user-granted permission in the package's instance
// catalog; module-owned scopes are contributed by their modules at runtime.
const testScopeRead authz.Permission = "apikeytest:read"

func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := apperror.PlatformCatalog().Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
