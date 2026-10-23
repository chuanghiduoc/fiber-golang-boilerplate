package authz

import "testing"

func TestPermissionsForRole_AdminHasAll(t *testing.T) {
	got := PermissionsForRole(RoleAdmin)

	if len(got) != len(allPermissions) {
		t.Fatalf("admin has %d permissions, want %d", len(got), len(allPermissions))
	}
	for _, permission := range allPermissions {
		if _, ok := got[permission]; !ok {
			t.Errorf("admin missing permission %q", permission)
		}
	}
}

func TestPermissionsForRole_User(t *testing.T) {
	got := PermissionsForRole(RoleUser)

	want := []Permission{
		PermissionProfileRead,
		PermissionProfileWrite,
		PermissionFilesRead,
		PermissionFilesWrite,
		PermissionFilesDelete,
		PermissionSessionsRevoke,
	}
	if len(got) != len(want) {
		t.Fatalf("user has %d permissions, want %d: %v", len(got), len(want), got)
	}
	for _, permission := range want {
		if _, ok := got[permission]; !ok {
			t.Errorf("user missing permission %q", permission)
		}
	}

	denied := []Permission{
		PermissionAdminUsersRead,
		PermissionAdminUsersRole,
		PermissionAdminUsersBan,
		PermissionAdminFilesRead,
		PermissionAdminFilesDelete,
		PermissionAdminFilesRestore,
		PermissionAdminStatsRead,
	}
	for _, permission := range denied {
		if _, ok := got[permission]; ok {
			t.Errorf("user unexpectedly granted admin permission %q", permission)
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
		{"admin profile read", RoleAdmin, PermissionProfileRead, true},
		{"admin stats read", RoleAdmin, PermissionAdminStatsRead, true},
		{"admin files delete", RoleAdmin, PermissionAdminFilesDelete, true},
		{"user profile read", RoleUser, PermissionProfileRead, true},
		{"user files write", RoleUser, PermissionFilesWrite, true},
		{"user sessions revoke", RoleUser, PermissionSessionsRevoke, true},
		{"user denied admin users read", RoleUser, PermissionAdminUsersRead, false},
		{"user denied admin stats", RoleUser, PermissionAdminStatsRead, false},
		{"unknown role denied", "guest", PermissionProfileRead, false},
		{"empty role denied", "", PermissionProfileRead, false},
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
