package membership

import (
	"errors"
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// statusOf is the HTTP status a handler would return for err.
func statusOf(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return resolvedStatus(appErr)
	}
	return -1
}

// codeOf is the typed error code carried by err, or "".
func codeOf(err error) string {
	if appErr, ok := err.(*apperror.AppError); ok {
		return appErr.Code
	}
	return ""
}

// resolvedStatus renders the error's code through the test catalog, the same
// way the HTTP layer does.
func resolvedStatus(appErr *apperror.AppError) int {
	_, spec := fakes.Errors.Resolve(apperror.Code(appErr.Code))
	return spec.Status
}
