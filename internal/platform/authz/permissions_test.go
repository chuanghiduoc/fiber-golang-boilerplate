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

func TestPermissionsForRole_OwnerAndAdminHaveAllTenantPermissions(t *testing.T) {
	all := testCatalog.AllTenantPermissions()
	for _, role := range []string{RoleAdmin, RoleOwner} {
		got := testCatalog.PermissionsForRole(role)
		if len(got) != len(all) {
			t.Fatalf("%s has %d permissions, want %d", role, len(got), len(all))
		}
		for _, permission := range all {
			if _, ok := got[permission]; !ok {
				t.Errorf("%s missing permission %q", role, permission)
			}
		}
	}
}

func TestPermissionsForRole_PlatformAdminHasOnlyPlatformPermissions(t *testing.T) {
	got := testCatalog.PermissionsForRole(RolePlatformAdmin)
	all := testCatalog.AllPlatformPermissions()
	if len(got) != len(all) {
		t.Fatalf("platform_admin has %d permissions, want %d", len(got), len(all))
	}
	for _, permission := range all {
		if !testCatalog.IsPlatformPermission(permission) {
			t.Fatalf("%q is not platform-scoped", permission)
		}
		if _, ok := got[permission]; !ok {
			t.Errorf("platform_admin missing permission %q", permission)
		}
	}
	for _, permission := range testCatalog.AllTenantPermissions() {
		if _, ok := got[permission]; ok {
			t.Errorf("platform_admin unexpectedly has tenant permission %q", permission)
		}
	}
}

func TestSharedCatalogIsAdminOnly(t *testing.T) {
	user := testCatalog.PermissionsForRole(RoleMember)
	for _, permission := range sharedAdminPermissions {
		if !testCatalog.IsKnownPermission(permission) {
			t.Errorf("shared permission %q is not registered", permission)
		}
		if owner, _ := testCatalog.OwnerOf(permission); owner != platformOwner {
			t.Errorf("shared permission %q owner = %q, want %q", permission, owner, platformOwner)
		}
		if _, ok := user[permission]; ok {
			t.Errorf("user unexpectedly granted shared admin permission %q", permission)
		}
	}
}

func TestPermissionsForRole_User(t *testing.T) {
	got := testCatalog.PermissionsForRole(RoleMember)
	for _, permission := range []Permission{testPermissionRead, testPermissionWrite} {
		if _, ok := got[permission]; !ok {
			t.Errorf("user missing registered permission %q", permission)
		}
	}
}

func TestPermissionsForRole_UnknownRoleIsEmpty(t *testing.T) {
	for _, role := range []string{"", "guest", "superadmin"} {
		got := testCatalog.PermissionsForRole(role)
		if got == nil {
			t.Fatalf("testCatalog.PermissionsForRole(%q) = nil, want empty map", role)
		}
		if len(got) != 0 {
			t.Errorf("testCatalog.PermissionsForRole(%q) = %v, want empty", role, got)
		}
	}
}

func TestPermissionsForRole_ReturnsCopy(t *testing.T) {
	got := testCatalog.PermissionsForRole(RoleMember)
	got[PermissionAdminUsersBan] = struct{}{}

	if testCatalog.Has(RoleMember, PermissionAdminUsersBan) {
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
		{"user registered read", RoleMember, testPermissionRead, true},
		{"user denied admin users read", RoleMember, PermissionAdminUsersRead, false},
		{"user denied admin stats", RoleMember, PermissionAdminStatsRead, false},
		{"unknown role denied", "guest", testPermissionRead, false},
		{"empty role denied", "", testPermissionRead, false},
		{"unknown permission denied", RoleAdmin, Permission("unknown:capability"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := testCatalog.Has(tt.role, tt.permission); got != tt.want {
				t.Errorf("Has(%q, %q) = %v, want %v", tt.role, tt.permission, got, tt.want)
			}
		})
	}
}

func TestAddGrantsAdminImplicitlyAndUserExplicitly(t *testing.T) {
	const adminOnly Permission = "registertest:admin_only"
	const userToo Permission = "registertest:user_too"
	catalog := NewCatalog()
	provider := Provider{Owner: "registertest", Grants: []Grant{
		{Permission: adminOnly},
		{Permission: userToo, Roles: []string{RoleMember, RoleAdmin}},
	}}
	if err := catalog.Add(provider); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !catalog.Has(RoleAdmin, adminOnly) || catalog.Has(RoleMember, adminOnly) {
		t.Errorf("%q must be admin-only", adminOnly)
	}
	if !catalog.Has(RoleAdmin, userToo) || !catalog.Has(RoleMember, userToo) {
		t.Errorf("%q must be granted to user and admin", userToo)
	}
	if owner, ok := catalog.OwnerOf(userToo); !ok || owner != "registertest" {
		t.Errorf("OwnerOf(%q) = %q, %v", userToo, owner, ok)
	}

	// The identical addition is idempotent (apps built twice in tests).
	if err := catalog.Add(provider); err != nil {
		t.Fatalf("identical re-add must be a no-op: %v", err)
	}
}

func TestAddRejectsConflicts(t *testing.T) {
	const owned Permission = "conflicttest:read"
	valid := []Grant{{Permission: owned, Roles: []string{RoleMember}}}
	catalog := NewCatalog()
	if err := catalog.Add(Provider{Owner: "conflicttest", Grants: valid}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	before := len(catalog.AllPermissions())

	cases := map[string]struct {
		owner  string
		grants []Grant
		want   string
	}{
		"other owner":       {"intruder", []Grant{{Permission: owned, Roles: []string{RoleMember}}}, "already owned"},
		"shared permission": {"intruder", []Grant{{Permission: PermissionAdminUsersRead}}, "already owned"},
		"changed roles":     {"conflicttest", []Grant{{Permission: owned}}, "re-registered"},
		"listed twice":      {"conflicttest", []Grant{{Permission: "conflicttest:x"}, {Permission: "conflicttest:x"}}, "twice"},
		"unknown role":      {"conflicttest", []Grant{{Permission: "conflicttest:y", Roles: []string{"guest"}}}, "unknown role"},
		"invalid id":        {"conflicttest", []Grant{{Permission: "Conflict Test"}}, "invalid permission"},
		"single segment":    {"conflicttest", []Grant{{Permission: "conflicttest"}}, "invalid permission"},
		"no owner":          {"", []Grant{{Permission: "conflicttest:z"}}, "owner is required"},
		// A failed call adds nothing, even its valid leading grants.
		"atomic": {"conflicttest", []Grant{{Permission: "conflicttest:valid"}, {Permission: "BAD"}}, "invalid permission"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := catalog.Add(Provider{Owner: tc.owner, Grants: tc.grants})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Add error = %v, want containing %q", err, tc.want)
			}
		})
	}
	if after := len(catalog.AllPermissions()); after != before {
		t.Fatalf("failed additions changed the catalog size from %d to %d", before, after)
	}
	if catalog.IsKnownPermission("conflicttest:valid") {
		t.Fatal("a failed Add call must not partially apply")
	}
}
