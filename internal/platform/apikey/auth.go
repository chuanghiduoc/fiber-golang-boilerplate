package apikey

import (
	"context"

	"github.com/google/uuid"
)

// APIKeyScheme is the Authorization scheme for API-key credentials.
const APIKeyScheme = "ApiKey"

// Authentication methods recorded on the request context and in authz
// attributes.
const (
	AuthMethodAPIKey = "api_key"
	AuthMethodJWT    = "jwt"
)

type apiKeyIDKey struct{}

type authMethodKey struct{}

// WithAPIKeyID records the authenticated key id in ctx.
func WithAPIKeyID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, apiKeyIDKey{}, id)
}

// APIKeyIDFromContext returns the authenticated key id, or uuid.Nil for a JWT
// request.
func APIKeyIDFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(apiKeyIDKey{}).(uuid.UUID)
	return id
}

// WithAuthMethod records how the request authenticated.
func WithAuthMethod(ctx context.Context, method string) context.Context {
	return context.WithValue(ctx, authMethodKey{}, method)
}

// AuthMethodFromContext returns auth_method_api_key or auth_method_jwt, or "".
func AuthMethodFromContext(ctx context.Context) string {
	method, _ := ctx.Value(authMethodKey{}).(string)
	return method
}
