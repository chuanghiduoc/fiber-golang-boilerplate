package authz

import (
	"strings"
	"testing"
)

var sharedAdminPermissions = []Permission{
	PermissionAdminUsersRead,
	PermissionAdminUsersWrite,
	PermissionAdminUsersDelete,
	PermissionAdminUsersRole,
	PermissionAdminUsersBan,
	PermissionAdminFilesRead,
	PermissionAdminFilesDelete,
	PermissionAdminFilesRestore,
	PermissionAdminStatsRead,
}

func TestPermissionsForRole_AdminHasAll(t *testing.T) {
	got := PermissionsForRole(RoleAdmin)
	all := AllPermissions()

	if len(got) != len(all) {
		t.Fatalf("admin has %d permissions, want %d", len(got), len(all))
	}
	for _, permission := range all {
		if _, ok := got[permission]; !ok {
			t.Errorf("admin missing permission %q", permission)
		}
	}
}

func TestSharedCatalogIsAdminOnly(t *testing.T) {
	user := PermissionsForRole(RoleUser)
	for _, permission := range sharedAdminPermissions {
		if !IsKnownPermission(permission) {
			t.Errorf("shared permission %q is not registered", permission)
		}
		if owner, _ := OwnerOf(permission); owner != platformOwner {
			t.Errorf("shared permission %q owner = %q, want %q", permission, owner, platformOwner)
		}
		if _, ok := user[permission]; ok {
			t.Errorf("user unexpectedly granted shared admin permission %q", permission)
		}
	}
}

func TestPermissionsForRole_User(t *testing.T) {
	got := PermissionsForRole(RoleUser)
	for _, permission := range []Permission{testPermissionRead, testPermissionWrite} {
		if _, ok := got[permission]; !ok {
			t.Errorf("user missing registered permission %q", permission)
		}
	}
}

func TestPermissionsForRole_UnknownRoleIsEmpty(t *testing.T) {
	for _, role := range []string{"", "guest", "superadmin"} {
		got := PermissionsForRole(role)
		if got == nil {
			t.Fatalf("PermissionsForRole(%q) = nil, want empty map", role)
		}
		if len(got) != 0 {
			t.Errorf("PermissionsForRole(%q) = %v, want empty", role, got)
		}
	}
}

func TestPermissionsForRole_ReturnsCopy(t *testing.T) {
	got := PermissionsForRole(RoleUser)
	got[PermissionAdminUsersBan] = struct{}{}

	if Has(RoleUser, PermissionAdminUsersBan) {
		t.Error("mutating the returned map must not alter the role catalog")
	}
}

func TestHas(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		permission Permission
		want       bool
	}{
		{"admin registered read", RoleAdmin, testPermissionRead, true},
		{"admin stats read", RoleAdmin, PermissionAdminStatsRead, true},
		{"admin files delete", RoleAdmin, PermissionAdminFilesDelete, true},
		{"user registered read", RoleUser, testPermissionRead, true},
		{"user denied admin users read", RoleUser, PermissionAdminUsersRead, false},
		{"user denied admin stats", RoleUser, PermissionAdminStatsRead, false},
		{"unknown role denied", "guest", testPermissionRead, false},
		{"empty role denied", "", testPermissionRead, false},
		{"unknown permission denied", RoleAdmin, Permission("unknown:capability"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Has(tt.role, tt.permission); got != tt.want {
				t.Errorf("Has(%q, %q) = %v, want %v", tt.role, tt.permission, got, tt.want)
			}
		})
	}
}

func TestRegisterGrantsAdminImplicitlyAndUserExplicitly(t *testing.T) {
	const adminOnly Permission = "registertest:admin_only"
	const userToo Permission = "registertest:user_too"
	if err := Register("registertest",
		Grant{Permission: adminOnly},
		Grant{Permission: userToo, Roles: []string{RoleUser, RoleAdmin}},
	); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !Has(RoleAdmin, adminOnly) || Has(RoleUser, adminOnly) {
		t.Errorf("%q must be admin-only", adminOnly)
	}
	if !Has(RoleAdmin, userToo) || !Has(RoleUser, userToo) {
		t.Errorf("%q must be granted to user and admin", userToo)
	}
	if owner, ok := OwnerOf(userToo); !ok || owner != "registertest" {
		t.Errorf("OwnerOf(%q) = %q, %v", userToo, owner, ok)
	}

	// The identical registration is idempotent (apps built twice in tests).
	if err := Register("registertest",
		Grant{Permission: adminOnly},
		Grant{Permission: userToo, Roles: []string{RoleUser}},
	); err != nil {
		t.Fatalf("identical re-registration must be a no-op: %v", err)
	}
}

func TestRegisterRejectsConflicts(t *testing.T) {
	const owned Permission = "conflicttest:read"
	if err := Register("conflicttest", Grant{Permission: owned, Roles: []string{RoleUser}}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	before := len(AllPermissions())

	cases := map[string]struct {
		owner  string
		grants []Grant
		want   string
	}{
		"other owner":       {"intruder", []Grant{{Permission: owned, Roles: []string{RoleUser}}}, "already owned"},
		"shared permission": {"intruder", []Grant{{Permission: PermissionAdminUsersRead}}, "already owned"},
		"changed roles":     {"conflicttest", []Grant{{Permission: owned}}, "re-registered"},
		"listed twice":      {"conflicttest", []Grant{{Permission: "conflicttest:x"}, {Permission: "conflicttest:x"}}, "twice"},
		"unknown role":      {"conflicttest", []Grant{{Permission: "conflicttest:y", Roles: []string{"guest"}}}, "unknown role"},
		"invalid id":        {"conflicttest", []Grant{{Permission: "Conflict Test"}}, "invalid permission"},
		"single segment":    {"conflicttest", []Grant{{Permission: "conflicttest"}}, "invalid permission"},
		"no owner":          {"", []Grant{{Permission: "conflicttest:z"}}, "owner is required"},
		// A failed call registers nothing, even its valid leading grants.
		"atomic": {"conflicttest", []Grant{{Permission: "conflicttest:valid"}, {Permission: "BAD"}}, "invalid permission"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := Register(tc.owner, tc.grants...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Register error = %v, want containing %q", err, tc.want)
			}
		})
	}
	if after := len(AllPermissions()); after != before {
		t.Fatalf("failed registrations changed the catalog size from %d to %d", before, after)
	}
	if IsKnownPermission("conflicttest:valid") {
		t.Fatal("a failed Register call must not partially apply")
	}
}
