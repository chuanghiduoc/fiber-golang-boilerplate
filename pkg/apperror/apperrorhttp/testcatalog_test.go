package apperrorhttp

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Module-owned codes are registered at startup by their modules. These tests
// register stand-ins with the same shape so the rendering tests stay
// independent of any business module.
const (
	CodeAuthInvalidCredentials apperror.Code = "auth.invalid_credentials"
	CodeUserNotFound           apperror.Code = "user.not_found"
)

func init() {
	if err := apperror.Register("apperrorhttptest", map[apperror.Code]apperror.Spec{
		CodeAuthInvalidCredentials: {
			Status:  http.StatusUnauthorized,
			Title:   "Invalid credentials",
			DocPath: "/errors/auth-invalid-credentials",
		},
		CodeUserNotFound: {
			Status:  http.StatusNotFound,
			Title:   "User not found",
			DocPath: "/errors/user-not-found",
		},
	}); err != nil {
		panic(err)
	}
}
