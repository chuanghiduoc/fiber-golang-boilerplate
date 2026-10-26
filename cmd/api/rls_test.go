package main

import (
	"strings"
	"testing"
)

func TestRLSRoleStateUnsafe(t *testing.T) {
	cases := []struct {
		name  string
		state rlsRoleState
		want  bool
	}{
		{name: "dedicated non-superuser role", state: rlsRoleState{}, want: false},
		{name: "superuser", state: rlsRoleState{Superuser: true}, want: true},
		{name: "bypassrls", state: rlsRoleState{BypassRLS: true}, want: true},
		{name: "table owner", state: rlsRoleState{OwnsTables: true}, want: true},
		{name: "member of an exempt role", state: rlsRoleState{MemberOfExempt: true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.unsafe(); got != tc.want {
				t.Fatalf("unsafe() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRLSRoleError(t *testing.T) {
	cases := []struct {
		name    string
		state   rlsRoleState
		env     string
		wantErr bool
	}{
		{name: "safe role in production", state: rlsRoleState{}, env: "production"},
		{name: "superuser in production", state: rlsRoleState{Superuser: true}, env: "production", wantErr: true},
		{name: "bypassrls in production", state: rlsRoleState{BypassRLS: true}, env: "production", wantErr: true},
		{name: "table owner in production", state: rlsRoleState{OwnsTables: true}, env: "production", wantErr: true},
		{name: "member of exempt role in production", state: rlsRoleState{MemberOfExempt: true}, env: "production", wantErr: true},
		{name: "superuser locally", state: rlsRoleState{Superuser: true}, env: "local"},
		{name: "superuser in staging", state: rlsRoleState{Superuser: true}, env: "staging"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := rlsRoleError(tc.state, tc.env)
			if (err != nil) != tc.wantErr {
				t.Fatalf("rlsRoleError(%+v, %q) = %v, want error %v", tc.state, tc.env, err, tc.wantErr)
			}
		})
	}
}

func TestBypassRoleStateUnsafe(t *testing.T) {
	cases := []struct {
		name  string
		state bypassRoleState
		want  bool
	}{
		{name: "dedicated bypass role", state: bypassRoleState{BypassRLS: true}, want: false},
		{name: "no bypassrls", state: bypassRoleState{}, want: true},
		{name: "superuser", state: bypassRoleState{BypassRLS: true, Superuser: true}, want: true},
		{name: "table owner", state: bypassRoleState{BypassRLS: true, OwnsTables: true}, want: true},
		{name: "reachable from the normal role", state: bypassRoleState{BypassRLS: true, NormalIsMember: true}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.unsafe(); got != tc.want {
				t.Fatalf("unsafe() = %v, want %v", got, tc.want)
			}
		})
	}

	reasons := strings.Join((bypassRoleState{BypassRLS: true, Superuser: true, OwnsTables: true, NormalIsMember: true}).reasons(), ", ")
	for _, want := range []string{"superuser", "owns tables", "SET ROLE"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reasons() = %q, want it to mention %q", reasons, want)
		}
	}
	missing := strings.Join((bypassRoleState{}).reasons(), ", ")
	if !strings.Contains(missing, "does not have BYPASSRLS") {
		t.Errorf("reasons() = %q, want a missing-BYPASSRLS reason", missing)
	}
}

func TestRLSRoleStateReasons(t *testing.T) {
	state := rlsRoleState{Superuser: true, BypassRLS: true, OwnsTables: true, MemberOfExempt: true}
	reasons := strings.Join(state.reasons(), ", ")
	for _, want := range []string{"superuser", "BYPASSRLS", "owns tables", "member"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("reasons() = %q, want it to mention %q", reasons, want)
		}
	}
	if got := (rlsRoleState{}).reasons(); len(got) != 0 {
		t.Errorf("reasons() for a safe role = %v, want none", got)
	}
}
