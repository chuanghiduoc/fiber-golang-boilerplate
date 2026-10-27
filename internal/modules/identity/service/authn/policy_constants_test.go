package authn

import (
	"testing"
	"time"
)

// TestSecurityPolicyConstants pins the authentication policy values. Other
// tests derive their timing from these constants, so a change here must be a
// deliberate policy decision, not a side effect.
func TestSecurityPolicyConstants(t *testing.T) {
	cases := []struct {
		name      string
		got, want any
	}{
		{"failed logins per (email, IP) before a lockout", maxLoginAttempts, 5},
		{"consecutive failures before the global account lock (NIST 800-63B)", maxAccountLoginFailures, 100},
		{"lockout duration", lockoutDuration, 15 * time.Minute},
		{"second-factor guesses per budget window", maxMFAAttempts, 5},
		{"refresh rotation grace for concurrent clients", refreshRotationGrace, 10 * time.Second},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
