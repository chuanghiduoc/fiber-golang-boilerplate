// Package authz defines the authorization capability catalog, the mapping from
// roles to permissions, and Fiber middleware that enforces permissions on
// routes. It has no dependency on the transport DTOs so modules can share the
// catalog without importing handler-level types.
package authz

// Permission identifies a single capability that can be granted to a role.
// Values are stable snake_case identifiers usable as audit/i18n keys.
type Permission string

// Permission catalog covering the current application capabilities.
const (
	PermissionProfileRead  Permission = "profile:read"
	PermissionProfileWrite Permission = "profile:write"

	PermissionFilesRead    Permission = "files:read"
	PermissionFilesWrite   Permission = "files:write"
	PermissionFilesDelete  Permission = "files:delete"
	PermissionFilesRestore Permission = "files:restore"

	PermissionSessionsRevoke Permission = "sessions:revoke"

	PermissionAdminUsersRead    Permission = "admin:users:read"
	PermissionAdminUsersRole    Permission = "admin:users:role"
	PermissionAdminUsersBan     Permission = "admin:users:ban"
	PermissionAdminFilesRead    Permission = "admin:files:read"
	PermissionAdminFilesDelete  Permission = "admin:files:delete"
	PermissionAdminFilesRestore Permission = "admin:files:restore"
	PermissionAdminStatsRead    Permission = "admin:stats:read"

	PermissionAdminAPIKeysRead   Permission = "admin:api_keys:read"
	PermissionAdminAPIKeysCreate Permission = "admin:api_keys:create"
	PermissionAdminAPIKeysUpdate Permission = "admin:api_keys:update"
	PermissionAdminAPIKeysRevoke Permission = "admin:api_keys:revoke"
	PermissionAdminAPIKeysRotate Permission = "admin:api_keys:rotate"
)

// Role identifiers. Kept local so authz does not import module DTOs.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// allPermissions is the canonical permission catalog. Admin receives every
// entry by construction, so adding a permission here grants it to admins only.
var allPermissions = []Permission{
	PermissionProfileRead,
	PermissionProfileWrite,
	PermissionFilesRead,
	PermissionFilesWrite,
	PermissionFilesDelete,
	PermissionFilesRestore,
	PermissionSessionsRevoke,
	PermissionAdminUsersRead,
	PermissionAdminUsersRole,
	PermissionAdminUsersBan,
	PermissionAdminFilesRead,
	PermissionAdminFilesDelete,
	PermissionAdminFilesRestore,
	PermissionAdminStatsRead,
	PermissionAdminAPIKeysRead,
	PermissionAdminAPIKeysCreate,
	PermissionAdminAPIKeysUpdate,
	PermissionAdminAPIKeysRevoke,
	PermissionAdminAPIKeysRotate,
}

// rolePermissions is the built-in role catalog. An unknown role has no
// permissions and is therefore denied everything.
var rolePermissions = buildRolePermissions()

func buildRolePermissions() map[string]map[Permission]struct{} {
	admin := make(map[Permission]struct{}, len(allPermissions))
	for _, p := range allPermissions {
		admin[p] = struct{}{}
	}

	user := map[Permission]struct{}{
		PermissionProfileRead:    {},
		PermissionProfileWrite:   {},
		PermissionFilesRead:      {},
		PermissionFilesWrite:     {},
		PermissionFilesDelete:    {},
		PermissionFilesRestore:   {},
		PermissionSessionsRevoke: {},
	}

	return map[string]map[Permission]struct{}{
		RoleUser:  user,
		RoleAdmin: admin,
	}
}

// PermissionsForRole returns a copy of the permission set granted to role.
// Unknown roles yield an empty, non-nil map. Callers may mutate the result
// without affecting the shared catalog.
func PermissionsForRole(role string) map[Permission]struct{} {
	granted := rolePermissions[role]
	out := make(map[Permission]struct{}, len(granted))
	for p := range granted {
		out[p] = struct{}{}
	}
	return out
}

// Has reports whether role is granted permission p.
func Has(role string, p Permission) bool {
	_, ok := rolePermissions[role][p]
	return ok
}

// IsKnownPermission reports whether p is part of the published catalog. It is
// used by API-key creation to reject unknown scopes instead of silently
// creating a key that can never pass an authz.Require check.
func IsKnownPermission(p Permission) bool {
	for _, known := range allPermissions {
		if known == p {
			return true
		}
	}
	return false
}

// AllPermissions returns a copy of the canonical permission catalog in
// declaration order.
func AllPermissions() []Permission {
	out := make([]Permission, len(allPermissions))
	copy(out, allPermissions)
	return out
}
