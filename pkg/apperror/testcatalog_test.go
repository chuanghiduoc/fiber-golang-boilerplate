package apperror

import "net/http"

// Module-owned codes are registered at startup by their modules. These tests
// register stand-ins with the same shape so the rendering tests stay
// independent of any business module.
const (
	CodeAuthInvalidCredentials Code = "auth.invalid_credentials"
	CodeUserNotFound           Code = "user.not_found"
)

func init() {
	if err := Register("apperrortest", map[Code]Spec{
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
