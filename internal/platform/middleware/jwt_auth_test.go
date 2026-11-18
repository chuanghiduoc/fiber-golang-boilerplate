package middleware

import (
	"slices"
	"testing"
)

// The live session is the source of the request's amr. A step-up changes the
// session at once, and the access token is an older snapshot: a method the
// session dropped (a passkey method after a TOTP step-up) must not come back
// from the token.
func TestEffectiveAMR(t *testing.T) {
	cases := []struct {
		name    string
		token   []string
		session []string
		want    []string
	}{
		{name: "session adds a method", token: []string{"pwd"}, session: []string{"pwd", "hwk"}, want: []string{"pwd", "hwk"}},
		{name: "session dropped a method", token: []string{"pwd", "hwk"}, session: []string{"pwd", "otp"}, want: []string{"pwd", "otp"}},
		{name: "store without session methods keeps the token", token: []string{"otp"}, session: nil, want: []string{"otp"}},
		{name: "both empty", token: nil, session: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveAMR(tc.token, tc.session); !slices.Equal(got, tc.want) {
				t.Fatalf("effectiveAMR() = %v, want %v", got, tc.want)
			}
		})
	}
}
