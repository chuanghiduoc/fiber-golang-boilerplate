package identity

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
)

// registerPasskeyRoutes mounts the passkey ceremonies. Registration and
// management are JWT-only and audited by the service in its mutation
// transaction; login is public and returns the normal token pair.
func (m *Module) registerPasskeyRoutes(api *httpapi.API, deps app.Deps) {
	if m.passkeys == nil {
		return
	}

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        "/api/v1/me/mfa/passkeys/options",
		OperationID: "startPasskeyRegistration",
		Tags:        []string{meTag},
		Summary:     "Start passkey registration",
		Description: "Returns WebAuthn creation options for the signed-in user. The ceremony expires after five minutes and is single-use.",
	}, httpapi.Policy{
		Permission: identitycatalog.PermissionMFAWrite,
		JWTOnly:    true,
		RateLimit:  httpapi.RateLimitNormal,
		NoStore:    true,
	}, m.passkeys.StartRegistration)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        "/api/v1/me/mfa/passkeys",
		OperationID: "createPasskey",
		Tags:        []string{meTag},
		Summary:     "Register a passkey",
		Description: "Verifies a WebAuthn registration attestation and stores the credential. The response never contains key material.",
	}, httpapi.Policy{
		Permission: identitycatalog.PermissionMFAWrite,
		JWTOnly:    true,
		RateLimit:  httpapi.RateLimitNormal,
		NoStore:    true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "auth.passkey_registered"},
	}, m.passkeys.Create)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodDelete,
		Path:        "/api/v1/me/mfa/passkeys/{passkeyId}",
		OperationID: "deletePasskey",
		Tags:        []string{meTag},
		Summary:     "Delete a passkey",
		Description: "Removes one of the caller's passkeys. Another user's id behaves as not found.",
	}, httpapi.Policy{
		Permission: identitycatalog.PermissionMFAWrite,
		JWTOnly:    true,
		RateLimit:  httpapi.RateLimitNormal,
		Audit:      httpapi.AuditPolicy{Tier1Action: "auth.passkey_deleted"},
	}, m.passkeys.Delete)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/passkeys/options",
		OperationID: "startPasskeyLogin",
		Tags:        []string{authTag},
		Summary:     "Start passkey login",
		Description: "Returns a discoverable WebAuthn assertion challenge. The optional email is only a hint: the response is identical whether or not the account exists.",
	}, httpapi.Policy{
		Public:    true,
		RateLimit: httpapi.RateLimitStrict,
		NoStore:   true,
	}, m.passkeys.StartLogin)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/passkeys/verifications",
		OperationID: "completePasskeyLogin",
		Tags:        []string{authTag},
		Summary:     "Complete passkey login",
		Description: "Verifies a WebAuthn assertion and returns the normal access and refresh token pair. The session records hwk or swk and is freshly AAL2.",
	}, httpapi.Policy{
		Public:    true,
		RateLimit: httpapi.RateLimitStrict,
		NoStore:   true,
		Audit:     httpapi.AuditPolicy{Tier1Action: "auth.passkey_login"},
	}, m.passkeys.CompleteLogin)

	httpapi.Register(api, httpapi.Op{
		Method:      http.MethodPost,
		Path:        "/api/v1/me/mfa/step-up",
		OperationID: "stepUpMFA",
		Tags:        []string{meTag},
		Summary:     "Refresh MFA freshness",
		Description: "Verifies a TOTP/recovery code or a passkey assertion and stamps the caller's session as freshly second-factor authenticated. A passkey step-up also adds hwk/swk to the session's amr.",
	}, httpapi.Policy{
		Permission: identitycatalog.PermissionMFAWrite,
		JWTOnly:    true,
		RateLimit:  httpapi.RateLimitNormal,
		NoStore:    true,
		Audit:      httpapi.AuditPolicy{Tier1Action: "auth.mfa_step_up"},
	}, m.passkeys.StepUp)
}
