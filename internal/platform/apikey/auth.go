package apikey

// APIKeyScheme is the Authorization scheme for API-key credentials.
const APIKeyScheme = "ApiKey"

// Fiber locals populated on successful API-key authentication (see
// apikey/apikeyhttp.Authenticate). They are plain keys so readers need no
// framework import.
const (
	LocalAPIKeyID   = "api_key_id"
	LocalAuthMethod = "auth_method"
)

// Authentication methods recorded in locals/authz attributes.
const (
	AuthMethodAPIKey = "api_key"
	AuthMethodJWT    = "jwt"
)
