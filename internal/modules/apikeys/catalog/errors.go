package catalog

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Error codes owned by apikeys.
const (
	// CodeAPIKeyExpiryExceedsMaxLifetime: the requested expiresAt is later
	// than APIKEYS_MAX_LIFETIME allows.
	CodeAPIKeyExpiryExceedsMaxLifetime apperror.Code = "api_key.expiry_exceeds_max_lifetime"
)

// ErrorCodes returns the apikeys error catalog for app.ErrorProvider.
func ErrorCodes() map[apperror.Code]apperror.Spec {
	return map[apperror.Code]apperror.Spec{
		CodeAPIKeyExpiryExceedsMaxLifetime: {
			Status:  http.StatusBadRequest,
			Title:   "API key expiry exceeds the maximum lifetime",
			DocPath: "/errors/api-key-expiry-exceeds-max-lifetime",
		},
	}
}
