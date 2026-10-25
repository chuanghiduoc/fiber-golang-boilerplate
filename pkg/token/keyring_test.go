package token

import (
	"errors"
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
		UserID:   1,
		TenantID: 1,
		Role:     "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
}

func TestGenerate_StampsDerivedKeyID(t *testing.T) {
	tok, err := GenerateWithTenant(1, 1, "a@b.com", "user", activeSecret, 1)
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
		tok, err := GenerateWithTenant(7, 1, "a@b.com", "user", secret, 1)
		if err != nil {
			t.Fatalf("GenerateWithTenant: %v", err)
		}
		claims, err := ring.Parse(tok)
		if err != nil {
			t.Fatalf("Parse token signed with %q: %v", secret[:6], err)
		}
		if claims.UserID != 7 {
			t.Fatalf("UserID = %d, want 7", claims.UserID)
		}
	}

	retired, _ := GenerateWithTenant(7, 1, "a@b.com", "user", "retired-secret-key-for-testing-00", 1)
	if _, err := ring.Parse(retired); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("token from a retired key: err = %v, want ErrUnknownKeyID", err)
	}
}

// Tokens issued before kid headers existed verify against the active key only.
func TestKeyring_TokenWithoutKeyIDUsesActiveKey(t *testing.T) {
	ring := NewKeyring(activeSecret, previousSecret)

	legacyActive, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims()).SignedString([]byte(activeSecret))
	if _, err := ring.Parse(legacyActive); err != nil {
		t.Fatalf("kid-less token signed with the active key must verify: %v", err)
	}

	legacyPrevious, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims()).SignedString([]byte(previousSecret))
	if _, err := ring.Parse(legacyPrevious); err == nil {
		t.Fatal("kid-less token signed with a previous key must be rejected")
	}
}

func TestKeyring_RejectsForgedKeyID(t *testing.T) {
	ring := NewKeyring(activeSecret, previousSecret)

	// A token claiming the active kid but signed with another secret.
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
	forged.Header["kid"] = KeyID(activeSecret)
	tok, _ := forged.SignedString([]byte("attacker-secret-attacker-secret-0"))
	if _, err := ring.Parse(tok); err == nil {
		t.Fatal("token with a known kid but a wrong signature must be rejected")
	}

	nonString := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims())
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
	expired.Header["kid"] = KeyID(previousSecret)
	tok, _ := expired.SignedString([]byte(previousSecret))
	if _, err := ring.Parse(tok); !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("expired token from a previous key: err = %v, want ErrTokenExpired", err)
	}
}
