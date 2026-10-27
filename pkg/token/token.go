package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims represents the JWT claims used across the application. It carries no
// email claim: RFC 9068 access tokens stay minimal, and a token holder must not
// be able to read a stale address from it.
type Claims struct {
	UserID   uuid.UUID `json:"user_id"`
	TenantID uuid.UUID `json:"tenant_id,omitempty"`
	// SessionID is the sign-in session (sessions.id) the token belongs to.
	SessionID uuid.UUID `json:"sid,omitempty"`
	// Authorization comes from the session's permission snapshot, never from
	// the token: a legacy "role" claim is ignored.
	TokenVersion int32 `json:"token_version"`
	// AMR is the Authentication Methods References set (RFC 8176): pwd,
	// google, otp, rc.
	AMR []string `json:"amr,omitempty"`
	jwt.RegisteredClaims
}

const (
	jwtIssuer   = "fiber-golang-boilerplate"
	jwtAudience = "fiber-golang-boilerplate-api"
	// accessTokenType is the RFC 9068 "typ" header for OAuth 2.0 access
	// tokens. Every token this package issues carries it, and Parse rejects a
	// token without it (pre-RFC-9068 tokens must be rotated).
	accessTokenType = "at+jwt"
)

// Generate creates a signed JWT token with token version 0 and no tenant
// claim. Prefer GenerateWithVersion in production so password changes, bans,
// and role changes can invalidate existing access tokens, and
// GenerateWithVersionAndTenant so the token is bound to its tenant.
func Generate(userID uuid.UUID, secret string, ttl time.Duration) (string, error) {
	return GenerateWithVersion(userID, 0, secret, ttl)
}

// GenerateWithTenant creates a signed JWT token carrying the user's tenant.
func GenerateWithTenant(userID, tenantID uuid.UUID, secret string, ttl time.Duration) (string, error) {
	return GenerateWithVersionAndTenant(userID, tenantID, 0, secret, ttl)
}

// GenerateWithVersion creates a signed JWT token carrying the user's current
// token version. It does not bind the token to a tenant or session.
func GenerateWithVersion(userID uuid.UUID, tokenVersion int32, secret string, ttl time.Duration) (string, error) {
	return GenerateWithVersionAndTenant(userID, uuid.Nil, tokenVersion, secret, ttl)
}

// GenerateWithVersionAndTenant creates a signed JWT token carrying both the
// user's current token version and tenant ID. A nil tenant ID produces a token without a tenant claim.
func GenerateWithVersionAndTenant(userID, tenantID uuid.UUID, tokenVersion int32, secret string, ttl time.Duration) (string, error) {
	return GenerateForSession(SessionClaims{
		UserID:       userID,
		TenantID:     tenantID,
		TokenVersion: tokenVersion,
	}, secret, ttl)
}

// SessionClaims are the application claims of an access token issued for a
// sign-in session.
type SessionClaims struct {
	UserID       uuid.UUID
	TenantID     uuid.UUID
	SessionID    uuid.UUID
	TokenVersion int32
	AMR          []string
}

// GenerateForSession creates a signed JWT bound to a user, tenant, and
// session. The session validator rejects a token whose session was revoked;
// a zero SessionID produces a token without the sid claim, which the
// validator rejects fail-closed.
func GenerateForSession(c SessionClaims, secret string, ttl time.Duration) (string, error) {
	claims := Claims{
		UserID:       c.UserID,
		TenantID:     c.TenantID,
		SessionID:    c.SessionID,
		TokenVersion: c.TokenVersion,
		AMR:          c.AMR,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}

	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	// The kid header lets a Keyring pick the verification key during a
	// JWT_SECRET rotation. It is derived from the secret, so every replica
	// stamps the same kid for the same key without coordination.
	t.Header[kidHeader] = KeyID(secret)
	t.Header["typ"] = accessTokenType
	return t.SignedString([]byte(secret))
}

// Parse validates a JWT token string signed with secret and returns the
// claims. It is a single-key Keyring.
func Parse(tokenString, secret string) (*Claims, error) {
	return NewKeyring(secret).Parse(tokenString)
}

const (
	kidHeader = "kid"
	// kidLength is the number of hex characters of the key fingerprint.
	kidLength = 8
	// kidDomain separates the kid fingerprint from any other use of the secret.
	kidDomain = "fiber-golang-boilerplate/jwt/kid/v1"
)

// Keyring errors.
var (
	ErrInvalidKeyID = errors.New("token kid header must be a string")
	// ErrMissingKeyID is returned for a token without the kid header. Every
	// current token records the key that signed it, so a kid-less token cannot
	// be attributed to a key and is rejected.
	ErrMissingKeyID = errors.New("token kid header is required")
	ErrUnknownKeyID = errors.New("token is signed with an unknown key")
	// ErrInvalidTokenType is returned for a token whose typ header is not
	// at+jwt (RFC 9068).
	ErrInvalidTokenType = errors.New("token typ header must be at+jwt")
)

// KeyID returns the kid stamped on tokens signed with secret: the first 8 hex
// characters of HMAC-SHA256(secret, domain). It discloses nothing an attacker
// does not already have (an HS256 token is itself an offline oracle for the
// secret) and is deterministic per key.
func KeyID(secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(kidDomain))
	return hex.EncodeToString(mac.Sum(nil))[:kidLength]
}

// Keyring verifies tokens signed with the active secret or with a previous
// secret kept during a rotation. Signing always uses the active secret (the
// Generate functions); previous secrets only verify.
//
// A token's kid header selects its key; a token without one is rejected, and
// so is an unknown kid. Distinct secrets that collide on the 32-bit kid are
// all tried, so a collision can never reject a valid token.
type Keyring struct {
	active []byte
	byKID  map[string][][]byte
}

// NewKeyring builds a keyring that verifies active plus every previous
// secret. Empty previous secrets and duplicates are ignored.
func NewKeyring(active string, previous ...string) *Keyring {
	k := &Keyring{active: []byte(active), byKID: make(map[string][][]byte, 1+len(previous))}
	seen := make(map[string]struct{}, 1+len(previous))
	secrets := append([]string{active}, previous...)
	for i, secret := range secrets {
		if _, dup := seen[secret]; dup || (i > 0 && secret == "") {
			continue
		}
		seen[secret] = struct{}{}
		kid := KeyID(secret)
		k.byKID[kid] = append(k.byKID[kid], []byte(secret))
	}
	return k
}

// candidates returns the secrets that may have signed a token with header.
func (k *Keyring) candidates(header map[string]any) ([][]byte, error) {
	typ, _ := header["typ"].(string)
	if typ != accessTokenType {
		return nil, ErrInvalidTokenType
	}
	raw, present := header[kidHeader]
	if !present {
		return nil, ErrMissingKeyID
	}
	kid, ok := raw.(string)
	if !ok {
		return nil, ErrInvalidKeyID
	}
	secrets, ok := k.byKID[kid]
	if !ok {
		return nil, ErrUnknownKeyID
	}
	return secrets, nil
}

// Parse validates a JWT token string against the keyring and returns the
// claims.
func (k *Keyring) Parse(tokenString string) (*Claims, error) {
	// The header is read unverified only to select candidate keys; the
	// signature and claims are verified by parseWithSecret.
	unverified, _, err := jwt.NewParser().ParseUnverified(tokenString, &Claims{})
	if err != nil {
		return nil, err
	}
	secrets, err := k.candidates(unverified.Header)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, secret := range secrets {
		claims, err := parseWithSecret(tokenString, secret)
		if err == nil {
			return claims, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// parseWithSecret validates tokenString with one HS256 secret.
func parseWithSecret(tokenString string, secret []byte) (*Claims, error) {
	claims := &Claims{}
	t, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (any, error) {
		// Pin the algorithm to prevent algorithm-confusion attacks. Only HS256
		// tokens are ever issued by this service.
		if t.Method != jwt.SigningMethodHS256 {
			return nil, jwt.ErrSignatureInvalid
		}
		return secret, nil
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
