package authn

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// resolvedStatus renders the error's code through the test catalog, the same
// way the HTTP layer does.
func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := fakes.Errors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
