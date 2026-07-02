package token

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims represents the JWT claims used across the application.
type Claims struct {
	UserID int64  `json:"user_id"`
	Email  string `json:"email"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}

const (
	jwtIssuer   = "fiber-golang-boilerplate"
	jwtAudience = "fiber-golang-boilerplate-api"
)

// Generate creates a signed JWT token.
func Generate(userID int64, email, role, secret string, expireHour int) (string, error) {
	claims := Claims{
		UserID: userID,
		Email:  email,
		Role:   role,
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
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(secret), nil
	},
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(jwtAudience),
	)
	if err != nil {
		return nil, err
	}
	if !t.Valid {
		return nil, errors.New("token is invalid")
	}
	return claims, nil
}
