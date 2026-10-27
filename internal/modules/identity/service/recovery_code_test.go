package service

import (
	"bytes"
	"strings"
	"testing"
)

// NIST SP 800-63B lookup secrets need at least 112 bits; 15 bytes is 120.
func TestRecoveryCodesCarry120Bits(t *testing.T) {
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		t.Fatalf("newRecoveryCodes: %v", err)
	}
	if len(plain) != mfaRecoveryCodeCount || len(hashes) != mfaRecoveryCodeCount {
		t.Fatalf("codes = %d/%d, want %d", len(plain), len(hashes), mfaRecoveryCodeCount)
	}
	seen := make(map[string]struct{}, len(plain))
	for i, code := range plain {
		groups := strings.Split(code, "-")
		if len(groups) != 6 {
			t.Fatalf("code %q = %d groups, want 6 (24 characters in groups of 4)", code, len(groups))
		}
		for _, group := range groups {
			if len(group) != 4 {
				t.Fatalf("code %q has group %q of length %d, want 4", code, group, len(group))
			}
		}
		normalized := normalizeRecoveryCode(code)
		if len(normalized) != 24 {
			t.Fatalf("normalized code = %q (%d chars), want 24 (120 bits in base32)", normalized, len(normalized))
		}
		if _, dup := seen[normalized]; dup {
			t.Fatalf("duplicate recovery code %q", normalized)
		}
		seen[normalized] = struct{}{}
		if !bytes.Equal(hashes[i], hashToken(normalized)) {
			t.Fatalf("hash %d does not match SHA-256 of the normalized code", i)
		}
		if len(hashes[i]) != 32 {
			t.Fatalf("hash %d is %d bytes, want SHA-256 (32)", i, len(hashes[i]))
		}
	}
}
