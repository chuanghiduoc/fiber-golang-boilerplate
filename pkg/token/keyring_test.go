package token

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	activeSecret   = "active-secret-key-for-testing-000"
	previousSecret = "previous-secret-key-for-testing-0"
)

func validClaims() Claims {
	return Claims{
		UserID:   testID(1),
		TenantID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
}

func TestGenerate_StampsDerivedKeyID(t *testing.T) {
	tok, err := GenerateWithTenant(testID(1), testID(1), activeSecret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateWithTenant: %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(tok, &Claims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	if got, want := parsed.Header["kid"], KeyID(activeSecret); got != want {
		t.Fatalf("kid = %v, want %q", got, want)
	}
	if len(KeyID(activeSecret)) != kidLength || KeyID(activeSecret) == KeyID(previousSecret) {
		t.Fatal("KeyID must be an 8-character fingerprint that differs per secret")
	}
}

func TestKeyring_VerifiesActiveAndPreviousKeys(t *testing.T) {
	ring := NewKeyring(activeSecret, previousSecret)

	for _, secret := range []string{activeSecret, previousSecret} {
		tok, err := GenerateWithTenant(testID(7), testID(1), secret, time.Hour)
		if err != nil {
			t.Fatalf("GenerateWithTenant: %v", err)
		}
		claims, err := ring.Parse(tok)
		if err != nil {
			t.Fatalf("Parse token signed with %q: %v", secret[:6], err)
		}
		if claims.UserID != testID(7) {
			t.Fatalf("UserID = %d, want 7", claims.UserID)
		}
	}

	retired, _ := GenerateWithTenant(testID(7), testID(1), "retired-secret-key-for-testing-00", time.Hour)
	if _, err := ring.Parse(retired); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("token from a retired key: err = %v, want ErrUnknownKeyID", err)
	}
}

// Tokens must name their signing key: a kid-less token is rejected even when
// the active secret would verify its signature.
func TestKeyring_RejectsTokenWithoutKeyID(t *testing.T) {
	ring := NewKeyring(activeSecret, previousSecret)

	kidlessActiveToken := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	kidlessActiveToken.Header["typ"] = accessTokenType
	kidlessActive, _ := kidlessActiveToken.SignedString([]byte(activeSecret))
	if _, err := ring.Parse(kidlessActive); err == nil {
		t.Fatal("a kid-less token signed with the active key must be rejected")
	}

	kidlessPreviousToken := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	kidlessPreviousToken.Header["typ"] = accessTokenType
	kidlessPrevious, _ := kidlessPreviousToken.SignedString([]byte(previousSecret))
	if _, err := ring.Parse(kidlessPrevious); err == nil {
		t.Fatal("a kid-less token signed with a previous key must be rejected")
	}
}

// A legacy role claim no longer reaches authorization: it is not part of the
// parsed claims and is never re-emitted.
func TestParse_IgnoresLegacyRoleClaim(t *testing.T) {
	legacy := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":   testID(1).String(),
		"tenant_id": testID(1).String(),
		"email":     "a@b.com",
		"role":      "admin",
		"iss":       jwtIssuer,
		"aud":       jwt.ClaimStrings{jwtAudience},
		"exp":       time.Now().Add(time.Hour).Unix(),
		"iat":       time.Now().Unix(),
	})
	legacy.Header["kid"] = KeyID(activeSecret)
	legacy.Header["typ"] = accessTokenType
	tok, err := legacy.SignedString([]byte(activeSecret))
	if err != nil {
		t.Fatalf("sign legacy token: %v", err)
	}

	claims, err := NewKeyring(activeSecret).Parse(tok)
	if err != nil {
		t.Fatalf("a token carrying a legacy role claim must still verify: %v", err)
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	if strings.Contains(string(encoded), `"role"`) {
		t.Fatalf("claims still expose the legacy role claim: %s", encoded)
	}
}

func TestKeyring_RejectsForgedKeyID(t *testing.T) {
	ring := NewKeyring(activeSecret, previousSecret)

	// A token claiming the active kid but signed with another secret.
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	forged.Header["typ"] = accessTokenType
	forged.Header["kid"] = KeyID(activeSecret)
	tok, _ := forged.SignedString([]byte("attacker-secret-attacker-secret-0"))
	if _, err := ring.Parse(tok); err == nil {
		t.Fatal("token with a known kid but a wrong signature must be rejected")
	}

	nonString := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	nonString.Header["typ"] = accessTokenType
	nonString.Header["kid"] = 42
	tok, _ = nonString.SignedString([]byte(activeSecret))
	if _, err := ring.Parse(tok); !errors.Is(err, ErrInvalidKeyID) {
		t.Fatalf("non-string kid: err = %v, want ErrInvalidKeyID", err)
	}
}

func TestKeyring_StillValidatesClaims(t *testing.T) {
	ring := NewKeyring(activeSecret, previousSecret)
	claims := validClaims()
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
	claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Hour))
	expired := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	expired.Header["typ"] = accessTokenType
	expired.Header["kid"] = KeyID(previousSecret)
	tok, _ := expired.SignedString([]byte(previousSecret))
	if _, err := ring.Parse(tok); !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("expired token from a previous key: err = %v, want ErrTokenExpired", err)
	}
}
