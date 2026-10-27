package middleware

import (
	"testing"
)

func TestUnionAMR(t *testing.T) {
	cases := []struct {
		name    string
		token   []string
		session []string
		want    []string
	}{
		{name: "session adds a method", token: []string{"pwd"}, session: []string{"pwd", "hwk"}, want: []string{"pwd", "hwk"}},
		{name: "no session state keeps token", token: []string{"otp"}, session: nil, want: []string{"otp"}},
		{name: "no duplicate", token: []string{"pwd", "hwk"}, session: []string{"hwk", "swk"}, want: []string{"pwd", "hwk", "swk"}},
		{name: "both empty", token: nil, session: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unionAMR(tc.token, tc.session)
			if len(got) != len(tc.want) {
				t.Fatalf("unionAMR() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("unionAMR() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
