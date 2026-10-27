package token

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "test-secret-key-for-testing"

func TestGenerateAndParse(t *testing.T) {
	tok, err := Generate(testID(42), testSecret, time.Hour)
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

	if claims.UserID != testID(42) {
		t.Errorf("UserID = %d, want 42", claims.UserID)
	}
	if claims.TenantID != uuid.Nil {
		t.Errorf("TenantID = %s, want nil UUID (legacy Generate)", claims.TenantID)
	}
	if claims.Issuer != jwtIssuer {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, jwtIssuer)
	}
	aud := claims.Audience
	if len(aud) != 1 || aud[0] != jwtAudience {
		t.Errorf("Audience = %v, want [%q]", aud, jwtAudience)
	}
}

// RFC 9068: every access token carries typ=at+jwt and nothing resembles an
// email claim.
func TestGenerate_SetsAccessTokenTypeAndOmitsEmail(t *testing.T) {
	tok, err := GenerateWithTenant(testID(42), testID(7), testSecret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateWithTenant: %v", err)
	}
	parsed, _, err := jwt.NewParser().ParseUnverified(tok, &Claims{})
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	if got := parsed.Header["typ"]; got != accessTokenType {
		t.Fatalf("typ header = %v, want %q", got, accessTokenType)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	if strings.Contains(string(encoded), `"email"`) {
		t.Fatalf("claims still carry an email: %s", encoded)
	}
}

func TestGenerateWithTenant(t *testing.T) {
	tok, err := GenerateWithTenant(testID(42), testID(7), testSecret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateWithTenant: %v", err)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TenantID != testID(7) {
		t.Errorf("TenantID = %d, want 7", claims.TenantID)
	}
	if claims.TokenVersion != 0 {
		t.Errorf("TokenVersion = %d, want 0", claims.TokenVersion)
	}
}

func TestGenerateWithVersionAndTenant(t *testing.T) {
	tok, err := GenerateWithVersionAndTenant(testID(42), testID(7), 3, testSecret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateWithVersionAndTenant: %v", err)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TenantID != testID(7) {
		t.Errorf("TenantID = %d, want 7", claims.TenantID)
	}
	if claims.TokenVersion != 3 {
		t.Errorf("TokenVersion = %d, want 3", claims.TokenVersion)
	}
}

func TestGenerateWithVersion_OmitsTenant(t *testing.T) {
	tok, err := GenerateWithVersion(testID(42), 2, testSecret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateWithVersion: %v", err)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.TenantID != uuid.Nil {
		t.Errorf("TenantID = %s, want nil UUID", claims.TenantID)
	}
	if claims.TokenVersion != 2 {
		t.Errorf("TokenVersion = %d, want 2", claims.TokenVersion)
	}
}

func TestGenerateWithVersionAndTenant_AllowsNoTenant(t *testing.T) {
	if _, err := GenerateWithVersionAndTenant(testID(42), uuid.Nil, 0, testSecret, time.Hour); err != nil {
		t.Fatalf("a token without a tenant must still be mintable: %v", err)
	}
}

func TestGenerateForSession_CarriesSessionID(t *testing.T) {
	tok, err := GenerateForSession(SessionClaims{
		UserID: testID(42), TenantID: testID(3), SessionID: testID(99), TokenVersion: 5,
	}, testSecret, time.Hour)
	if err != nil {
		t.Fatalf("GenerateForSession: %v", err)
	}
	claims, err := Parse(tok, testSecret)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if claims.UserID != testID(42) || claims.TenantID != testID(3) || claims.SessionID != testID(99) || claims.TokenVersion != 5 {
		t.Fatalf("claims = %+v", claims)
	}
	if _, err := GenerateForSession(SessionClaims{UserID: testID(1), TenantID: testID(1), SessionID: uuid.Nil}, testSecret, time.Hour); err != nil {
		t.Fatalf("a token without a session must still be mintable: %v", err)
	}
	legacy, _ := GenerateWithVersionAndTenant(testID(1), testID(1), 0, testSecret, time.Hour)
	if claims, _ := Parse(legacy, testSecret); claims.SessionID != uuid.Nil {
		t.Fatalf("a token without a session has sid %s", claims.SessionID)
	}
}

func TestParse_WrongSecret(t *testing.T) {
	tok, _ := Generate(testID(1), testSecret, time.Hour)
	_, err := Parse(tok, "wrong-secret")
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestParse_ExpiredToken(t *testing.T) {
	claims := Claims{
		UserID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
	tok := signClaims(t, claims, testSecret)
	_, err := Parse(tok, testSecret)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestParse_WrongIssuer(t *testing.T) {
	claims := Claims{
		UserID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "wrong-issuer",
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
	tok := signClaims(t, claims, testSecret)
	_, err := Parse(tok, testSecret)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
}

func TestParse_WrongAudience(t *testing.T) {
	claims := Claims{
		UserID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{"wrong-audience"},
		},
	}
	tok := signClaims(t, claims, testSecret)
	_, err := Parse(tok, testSecret)
	if err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestParse_RequiresExpiration(t *testing.T) {
	claims := Claims{
		UserID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(time.Now()),
			Issuer:   jwtIssuer,
			Audience: jwt.ClaimStrings{jwtAudience},
		},
	}
	tok := signClaims(t, claims, testSecret)
	if _, err := Parse(tok, testSecret); err == nil {
		t.Fatal("token without exp must be rejected")
	}
}

func TestParse_RejectsNonHS256Algorithms(t *testing.T) {
	claims := Claims{
		UserID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
	signed := jwt.NewWithClaims(jwt.SigningMethodHS384, claims)
	signed.Header["kid"] = KeyID(testSecret)
	signed.Header["typ"] = accessTokenType
	tok, _ := signed.SignedString([]byte(testSecret))
	if _, err := Parse(tok, testSecret); err == nil {
		t.Fatal("HS384 token must be rejected when HS256 is expected")
	}
}

// A token without typ=at+jwt is not an access token this service accepts.
func TestParse_RejectsMissingTokenType(t *testing.T) {
	raw := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaimsForTypeTest())
	raw.Header["kid"] = KeyID(testSecret)
	typeless, _ := raw.SignedString([]byte(testSecret))
	if _, err := Parse(typeless, testSecret); !errors.Is(err, ErrInvalidTokenType) {
		t.Fatalf("typ-less token: err = %v, want ErrInvalidTokenType", err)
	}
}

func validClaimsForTypeTest() Claims {
	return Claims{
		UserID: testID(1),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    jwtIssuer,
			Audience:  jwt.ClaimStrings{jwtAudience},
		},
	}
}

func signClaims(t *testing.T, claims Claims, secret string) string {
	t.Helper()
	signed := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed.Header["kid"] = KeyID(secret)
	signed.Header["typ"] = accessTokenType
	tok, err := signed.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	return tok
}
