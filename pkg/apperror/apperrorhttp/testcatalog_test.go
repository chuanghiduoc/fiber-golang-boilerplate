package apperrorhttp

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Module-owned codes are contributed at the composition root. These tests use
// a stand-in catalog with the same shape so the rendering tests stay
// independent of any business module.
const (
	CodeAuthInvalidCredentials apperror.Code = "auth.invalid_credentials"
	CodeUserNotFound           apperror.Code = "user.not_found"
)

var testCatalog = newTestCatalog()

func newTestCatalog() *apperror.Catalog {
	catalog := apperror.NewCatalog()
	if err := catalog.Add(apperror.Provider{Owner: "apperrorhttptest", Specs: []apperror.Spec{
		{
			Code:    CodeAuthInvalidCredentials,
			Status:  http.StatusUnauthorized,
			Title:   "Invalid credentials",
			DocPath: "/errors/auth-invalid-credentials",
		},
		{
			Code:    CodeUserNotFound,
			Status:  http.StatusNotFound,
			Title:   "User not found",
			DocPath: "/errors/user-not-found",
		},
	}}); err != nil {
		panic(err)
	}
	catalog.Seal()
	return catalog
}
