package authz

import (
	"testing"

	"github.com/google/uuid"
)

func TestSubjectMFAAndPasskey(t *testing.T) {
	cases := []struct {
		name       string
		amr        []string
		hasMFA     bool
		hasPasskey bool
	}{
		{name: "no methods", amr: nil, hasMFA: false, hasPasskey: false},
		{name: "password only", amr: []string{"pwd"}, hasMFA: false, hasPasskey: false},
		{name: "totp", amr: []string{"pwd", "otp"}, hasMFA: true, hasPasskey: false},
		{name: "recovery code", amr: []string{"google", "rc"}, hasMFA: true, hasPasskey: false},
		{name: "hardware passkey", amr: []string{"hwk"}, hasMFA: true, hasPasskey: true},
		{name: "software passkey", amr: []string{"swk"}, hasMFA: true, hasPasskey: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject := NewSubject(uuid.Nil, uuid.Nil, RoleMember).WithAMR(tc.amr)
			if got := subject.HasMFA(); got != tc.hasMFA {
				t.Fatalf("HasMFA() = %v, want %v", got, tc.hasMFA)
			}
			if got := subject.HasPasskey(); got != tc.hasPasskey {
				t.Fatalf("HasPasskey() = %v, want %v", got, tc.hasPasskey)
			}
		})
	}
}
