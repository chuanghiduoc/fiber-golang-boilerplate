package mfa

import (
	"testing"
	"time"
)

// TestSecurityPolicyConstants pins the MFA policy values. Other tests derive
// their timing from these constants, so a change here must be a deliberate
// policy decision, not a side effect.
func TestSecurityPolicyConstants(t *testing.T) {
	cases := []struct {
		name      string
		got, want any
	}{
		{"MFA challenge lifetime", mfaChallengeTTL, 5 * time.Minute},
		{"pending TOTP enrollment lifetime", mfaEnrollmentTTL, 15 * time.Minute},
		{"session age that still counts as re-authenticated", mfaReauthSessionMaxAge, 15 * time.Minute},
		{"recovery codes issued per confirmation", mfaRecoveryCodeCount, 10},
		{"recovery code entropy in bytes (120 bits, NIST look-up secret)", mfaRecoveryCodeBytes, 15},
		{"passkey ceremony lifetime", PasskeyCeremonyTTL, 5 * time.Minute},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
