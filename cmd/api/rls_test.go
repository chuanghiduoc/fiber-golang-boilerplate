package main

import "testing"

func TestRLSRoleError(t *testing.T) {
	cases := []struct {
		name    string
		unsafe  bool
		env     string
		wantErr bool
	}{
		{name: "safe role in production", unsafe: false, env: "production"},
		{name: "superuser in production", unsafe: true, env: "production", wantErr: true},
		{name: "superuser locally", unsafe: true, env: "local"},
		{name: "superuser in staging", unsafe: true, env: "staging"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rlsRoleError(tc.unsafe, tc.env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("rlsRoleError(%v, %q) = %v, want error %v", tc.unsafe, tc.env, err, tc.wantErr)
			}
		})
	}
}
