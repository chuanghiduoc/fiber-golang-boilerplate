package token

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "test-secret-key-for-testing"

func TestGenerateAndParse(t *testing.T) {
	tok, err := Generate(42, "user@test.com", "admin", testSecret, 1)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if tok == "" {
		t.Fatal("Generate returned empty token")
	}

	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if claims.UserID != 42 {
		t.Errorf("UserID = %d, want 42", claims.UserID)
	}
	if claims.TenantID != 0 {
		t.Errorf("TenantID = %d, want 0 (legacy Generate)", claims.TenantID)
	}
	if claims.Email != "user@test.com" {
		t.Errorf("Email = %q, want %q", claims.Email, "user@test.com")
	}
	if claims.Role != "admin" {
		t.Errorf("Role = %q, want %q", claims.Role, "admin")
	}
	if claims.Issuer != jwtIssuer {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, jwtIssuer)
	}
	aud := claims.Audience
	if len(aud) != 1 || aud[0] != jwtAudience {
		t.Errorf("Audience = %v, want [%q]", aud, jwtAudience)
	}
}

func TestGenerateWithTenant(t *testing.T) {
	tok, err := GenerateWithTenant(42, 7, "user@test.com", "user", testSecret, 1)
	if err != nil {
		t.Fatalf("GenerateWithTenant: %v", err)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TenantID != 7 {
		t.Errorf("TenantID = %d, want 7", claims.TenantID)
	}
	if claims.TokenVersion != 0 {
		t.Errorf("TokenVersion = %d, want 0", claims.TokenVersion)
	}
}

func TestGenerateWithVersionAndTenant(t *testing.T) {
	tok, err := GenerateWithVersionAndTenant(42, 7, "user@test.com", "user", 3, testSecret, 1)
	if err != nil {
		t.Fatalf("GenerateWithVersionAndTenant: %v", err)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TenantID != 7 {
		t.Errorf("TenantID = %d, want 7", claims.TenantID)
	}
	if claims.TokenVersion != 3 {
		t.Errorf("TokenVersion = %d, want 3", claims.TokenVersion)
	}
}

func TestGenerateWithVersion_LegacyOmitsTenant(t *testing.T) {
	tok, err := GenerateWithVersion(42, "user@test.com", "user", 2, testSecret, 1)
	if err != nil {
		t.Fatalf("GenerateWithVersion: %v", err)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TenantID != 0 {
		t.Errorf("TenantID = %d, want 0", claims.TenantID)
	}
	if claims.TokenVersion != 2 {
		t.Errorf("TokenVersion = %d, want 2", claims.TokenVersion)
	}
}

func TestGenerateWithVersionAndTenant_RejectsNegativeTenant(t *testing.T) {
	if _, err := GenerateWithVersionAndTenant(42, -1, "user@test.com", "user", 0, testSecret, 1); err == nil {
		t.Fatal("negative tenant ID must be rejected")
	}
}

func TestParse_WrongSecret(t *testing.T) {
	tok, _ := Generate(1, "a@b.com", "user", testSecret, 1)
	_, err := Parse(tok, "wrong-secret")
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestParse_ExpiredToken(t *testing.T) {
	// Generate a token that's already expired
	claims := Claims{
		UserID: 1,
		Email:  "a@b.com",
		Role:   "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))

	_, err := Parse(tok, testSecret)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestParse_WrongIssuer(t *testing.T) {
	claims := Claims{
		UserID: 1,
		Email:  "a@b.com",
		Role:   "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "wrong-issuer",
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))

	_, err := Parse(tok, testSecret)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestParse_WrongAudience(t *testing.T) {
	claims := Claims{
		UserID: 1,
		Email:  "a@b.com",
		Role:   "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{"wrong-audience"},
		},
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))

	_, err := Parse(tok, testSecret)
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestParse_RequiresExpiration(t *testing.T) {
	claims := Claims{
		UserID: 1,
		Email:  "a@b.com",
		Role:   "user",
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(time.Now()),
			Issuer:   jwtIssuer,
			Audience: jwt.ClaimStrings{jwtAudience},
		},
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if _, err := Parse(tok, testSecret); err == nil {
		t.Fatal("token without exp must be rejected")
	}
}

func TestParse_RejectsNonHS256Algorithms(t *testing.T) {
	claims := Claims{
		UserID: 1,
		Email:  "a@b.com",
		Role:   "user",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS384, claims).SignedString([]byte(testSecret))
	if _, err := Parse(tok, testSecret); err == nil {
		t.Fatal("HS384 token must be rejected when HS256 is expected")
	}
}
