package totp

import (
	"encoding/base32"
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B vectors (SHA-1). The published codes are 8 digits; a
// 6-digit TOTP is the same truncated value modulo 10^6, i.e. its last six
// digits.
func TestCodeRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).
		EncodeToString([]byte("12345678901234567890"))
	vectors := []struct {
		unix int64
		code string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, v := range vectors {
		got, err := Code(secret, time.Unix(v.unix, 0))
		if err != nil {
			t.Fatalf("Code(%d): %v", v.unix, err)
		}
		if got != v.code {
			t.Errorf("Code(%d) = %q, want %q", v.unix, got, v.code)
		}
	}
}

func TestValidateAcceptsAdjacentStepsOnly(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	step := Step(now)

	for _, delta := range []int64{-1, 0, 1} {
		code, err := CodeAt(secret, step+delta)
		if err != nil {
			t.Fatalf("CodeAt: %v", err)
		}
		matched, ok := Validate(secret, code, now)
		if !ok || matched != step+delta {
			t.Errorf("Validate(delta=%d) = (%d,%v), want (%d,true)", delta, matched, ok, step+delta)
		}
	}
	for _, delta := range []int64{-2, 2, 10} {
		code, err := CodeAt(secret, step+delta)
		if err != nil {
			t.Fatalf("CodeAt: %v", err)
		}
		if _, ok := Validate(secret, code, now); ok {
			t.Errorf("Validate(delta=%d) accepted a code outside the window", delta)
		}
	}
}

func TestValidateRejectsMalformedCodes(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	for _, code := range []string{"", "12345", "1234567", "abcdef", " 123456"} {
		if _, ok := Validate(secret, code, now); ok {
			t.Errorf("Validate(%q) accepted a malformed code", code)
		}
	}
}

func TestGenerateSecretIsBase32AndRandom(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	b, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if a == b {
		t.Fatal("GenerateSecret returned the same secret twice")
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(a)
	if err != nil {
		t.Fatalf("secret is not base32: %v", err)
	}
	if len(raw) != 20 {
		t.Fatalf("secret entropy = %d bytes, want 20", len(raw))
	}
}

func TestURI(t *testing.T) {
	uri := URI("Example App", "user@example.com", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{
		"otpauth://totp/",
		"Example%20App:user@example.com",
		"secret=JBSWY3DPEHPK3PXP",
		"issuer=Example+App",
		"algorithm=SHA1",
		"digits=6",
		"period=30",
	} {
		if !strings.Contains(uri, want) {
			t.Errorf("URI() = %q, missing %q", uri, want)
		}
	}
}
