package token

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims represents the JWT claims used across the application.
type Claims struct {
	UserID       int64  `json:"user_id"`
	TenantID     int64  `json:"tenant_id,omitempty"`
	Email        string `json:"email"`
	Role         string `json:"role"`
	TokenVersion int32  `json:"token_version"`
	jwt.RegisteredClaims
}

const (
	jwtIssuer   = "fiber-golang-boilerplate"
	jwtAudience = "fiber-golang-boilerplate-api"
)

// Generate creates a signed JWT token with token version 0 and no tenant
// claim. Prefer GenerateWithVersion in production so password changes, bans,
// and role changes can invalidate existing access tokens, and
// GenerateWithVersionAndTenant so the token is bound to its tenant.
func Generate(userID int64, email, role, secret string, expireHour int) (string, error) {
	return GenerateWithVersion(userID, email, role, 0, secret, expireHour)
}

// GenerateWithTenant creates a signed JWT token carrying the user's tenant.
func GenerateWithTenant(userID, tenantID int64, email, role, secret string, expireHour int) (string, error) {
	return GenerateWithVersionAndTenant(userID, tenantID, email, role, 0, secret, expireHour)
}

// GenerateWithVersion creates a signed JWT token carrying the user's current
// token version. It omits the tenant claim for backward compatibility.
func GenerateWithVersion(userID int64, email, role string, tokenVersion int32, secret string, expireHour int) (string, error) {
	return GenerateWithVersionAndTenant(userID, 0, email, role, tokenVersion, secret, expireHour)
}

// GenerateWithVersionAndTenant creates a signed JWT token carrying both the
// user's current token version and tenant ID. A negative tenant ID is rejected;
// zero remains reserved for legacy tokens that predate multi-tenancy.
func GenerateWithVersionAndTenant(userID, tenantID int64, email, role string, tokenVersion int32, secret string, expireHour int) (string, error) {
	if tenantID < 0 {
		return "", errors.New("tenant ID must not be negative")
	}
	claims := Claims{
		UserID:       userID,
		TenantID:     tenantID,
		Email:        email,
		Role:         role,
		TokenVersion: tokenVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(expireHour) * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}

	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(secret))
}

// Parse validates a JWT token string and returns the claims.
func Parse(tokenString, secret string) (*Claims, error) {
	claims := &Claims{}
	t, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		// Pin the algorithm to prevent algorithm-confusion attacks. Only HS256
		// tokens are ever issued by this service.
		if t.Method != jwt.SigningMethodHS256 {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(secret), nil
	},
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(jwtAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, err
	}
	if !t.Valid {
		return nil, errors.New("token is invalid")
	}
	return claims, nil
}
