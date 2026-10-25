// Package authz defines the authorization capability catalog, the mapping from
// roles to permissions, and Fiber middleware that enforces permissions on
// routes. It has no dependency on the transport DTOs so modules can share the
// catalog without importing handler-level types.
//
// The catalog has two sources. Permissions checked by more than one module
// (the admin back-office permissions enforced both by the admin routes and by
// the identity/files services) are declared here. Every other permission is
// owned by one module and registered at startup through Register (wired by
// app.RegisterCatalogs from app.PermissionProvider). Owner and admin receive
// every tenant permission by construction.
package authz

import (
	"fmt"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
)

// Permission identifies a single capability that can be granted to a role.
// Values are stable identifiers usable as audit/i18n keys and API-key scopes.
type Permission string

// Shared permission catalog. These are checked by more than one module, so no
// single module can own them without a cross-module import:
//   - the admin module's routes require admin:users:* / admin:files:* /
//     admin:stats:read, and
//   - identity (user handler owner-or-admin checks) and files (admin service
//     ABAC checks) enforce the same values again below the route.
const (
	PermissionAdminUsersRead    Permission = "admin:users:read"
	PermissionAdminUsersWrite   Permission = "admin:users:write"
	PermissionAdminUsersDelete  Permission = "admin:users:delete"
	PermissionAdminUsersRole    Permission = "admin:users:role"
	PermissionAdminUsersBan     Permission = "admin:users:ban"
	PermissionAdminFilesRead    Permission = "admin:files:read"
	PermissionAdminFilesDelete  Permission = "admin:files:delete"
	PermissionAdminFilesRestore Permission = "admin:files:restore"
	PermissionAdminStatsRead    Permission = "admin:stats:read"

	// Platform permissions are process-wide and cross-tenant. Only the
	// platform_admin role receives them.
	PermissionPlatformTenantsRead   Permission = "platform:tenants:read"
	PermissionPlatformTenantsWrite  Permission = "platform:tenants:write"
	PermissionPlatformFeaturesWrite Permission = "platform:features:write"
)

// System role keys (roles.key). Member permissions come from the catalog;
// owner and admin receive every tenant permission by construction; platform
// permissions belong to platform_admin only.
const (
	RoleMember        = "member"
	RoleAdmin         = "admin"
	RoleOwner         = "owner"
	RolePlatformAdmin = "platform_admin"
)

// platformOwner is the owner recorded for the shared catalog above.
const platformOwner = "platform"

// Grant declares one permission and the built-in roles that receive it.
// Owner and admin always receive every tenant permission and need not be
// listed. A platform grant sets Platform and lists no roles: it is granted to
// platform_admin only.
type Grant struct {
	Permission Permission
	Roles      []string
	Platform   bool
}

// permissionPattern keeps identifiers stable and scope-friendly:
// lower-case segments separated by ':' (for example "orders:read" or
// "admin:api_keys:rotate").
var permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)+$`)

// catalogState is an immutable snapshot; Register publishes a new one so the
// per-request read path (Has) is a lock-free atomic load.
type catalogState struct {
	order    []Permission
	owners   map[Permission]string
	grants   map[Permission][]string
	platform map[Permission]struct{}
	roles    map[string]map[Permission]struct{}
}

var (
	registerMu sync.Mutex
	state      atomic.Pointer[catalogState]
)

func init() {
	shared := []Grant{
		{Permission: PermissionAdminUsersRead},
		{Permission: PermissionAdminUsersWrite},
		{Permission: PermissionAdminUsersDelete},
		{Permission: PermissionAdminUsersRole},
		{Permission: PermissionAdminUsersBan},
		{Permission: PermissionAdminFilesRead},
		{Permission: PermissionAdminFilesDelete},
		{Permission: PermissionAdminFilesRestore},
		{Permission: PermissionAdminStatsRead},
		{Permission: PermissionPlatformTenantsRead, Platform: true},
		{Permission: PermissionPlatformTenantsWrite, Platform: true},
		{Permission: PermissionPlatformFeaturesWrite, Platform: true},
	}
	state.Store(&catalogState{
		owners:   map[Permission]string{},
		grants:   map[Permission][]string{},
		platform: map[Permission]struct{}{},
		roles: map[string]map[Permission]struct{}{
			RoleMember:        {},
			RoleAdmin:         {},
			RoleOwner:         {},
			RolePlatformAdmin: {},
		},
	})
	if err := Register(platformOwner, shared...); err != nil {
		panic(err)
	}
}

// Register adds owner's permissions to the catalog. It fails on an invalid
// permission identifier, an unknown role, a permission listed twice, or a
// permission already registered by a different owner or with different
// roles. Re-registering the identical grants for the same owner is a no-op,
// so building the application more than once in a process (tests) is safe.
// Call it during startup, before serving requests.
func Register(owner string, grants ...Grant) error {
	if owner == "" {
		return fmt.Errorf("authz: register: owner is required")
	}
	registerMu.Lock()
	defer registerMu.Unlock()

	current := state.Load()
	next := current.clone()
	seen := make(map[Permission]struct{}, len(grants))
	for _, grant := range grants {
		p := grant.Permission
		if !permissionPattern.MatchString(string(p)) {
			return fmt.Errorf("authz: %s: invalid permission %q (want lower-case segments joined by ':')", owner, p)
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("authz: %s: permission %q listed twice", owner, p)
		}
		seen[p] = struct{}{}

		roles, err := normalizeRoles(grant)
		if err != nil {
			return fmt.Errorf("authz: %s: permission %q: %w", owner, p, err)
		}
		if existingOwner, exists := current.owners[p]; exists {
			if existingOwner != owner {
				return fmt.Errorf("authz: permission %q registered by %q is already owned by %q", p, owner, existingOwner)
			}
			_, wasPlatform := current.platform[p]
			if !slices.Equal(current.grants[p], roles) || wasPlatform != grant.Platform {
				return fmt.Errorf("authz: %s: permission %q re-registered with roles %v (was %v)", owner, p, roles, current.grants[p])
			}
			continue
		}
		next.order = append(next.order, p)
		next.owners[p] = owner
		next.grants[p] = roles
		if grant.Platform {
			next.platform[p] = struct{}{}
			next.roles[RolePlatformAdmin][p] = struct{}{}
		} else {
			next.roles[RoleAdmin][p] = struct{}{}
			next.roles[RoleOwner][p] = struct{}{}
			for _, role := range roles {
				next.roles[role][p] = struct{}{}
			}
		}
	}
	state.Store(next)
	return nil
}

// normalizeRoles validates a grant and returns the member roles it lists,
// without owner and admin, which are implicit.
func normalizeRoles(grant Grant) ([]string, error) {
	if grant.Platform {
		if len(grant.Roles) > 0 {
			return nil, fmt.Errorf("platform permissions cannot list roles")
		}
		return []string{}, nil
	}
	out := make([]string, 0, len(grant.Roles))
	for _, role := range grant.Roles {
		switch role {
		case RoleOwner, RoleAdmin:
			continue
		case RoleMember:
			if slices.Contains(out, role) {
				return nil, fmt.Errorf("role %q listed twice", role)
			}
			out = append(out, role)
		default:
			return nil, fmt.Errorf("unknown role %q", role)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *catalogState) clone() *catalogState {
	next := &catalogState{
		order:    slices.Clone(s.order),
		owners:   make(map[Permission]string, len(s.owners)),
		grants:   make(map[Permission][]string, len(s.grants)),
		platform: make(map[Permission]struct{}, len(s.platform)),
		roles:    make(map[string]map[Permission]struct{}, len(s.roles)),
	}
	for p, owner := range s.owners {
		next.owners[p] = owner
	}
	for p, roles := range s.grants {
		next.grants[p] = roles
	}
	for p := range s.platform {
		next.platform[p] = struct{}{}
	}
	for role, perms := range s.roles {
		copied := make(map[Permission]struct{}, len(perms))
		for p := range perms {
			copied[p] = struct{}{}
		}
		next.roles[role] = copied
	}
	return next
}

// PermissionsForRole returns a copy of the permission set granted to role.
// Unknown roles yield an empty, non-nil map. Callers may mutate the result
// without affecting the shared catalog.
func PermissionsForRole(role string) map[Permission]struct{} {
	granted := state.Load().roles[canonicalRole(role)]
	out := make(map[Permission]struct{}, len(granted))
	for p := range granted {
		out[p] = struct{}{}
	}
	return out
}

// Has reports whether role is granted permission p.
func Has(role string, p Permission) bool {
	_, ok := state.Load().roles[canonicalRole(role)][p]
	return ok
}

// canonicalRole maps the pre-RBAC "user" key, still present in tokens and
// old rows, to member.
func canonicalRole(role string) string {
	if role == "user" {
		return RoleMember
	}
	return role
}

// IsKnownPermission reports whether p is part of the published catalog. It is
// used by API-key creation to reject unknown scopes instead of silently
// creating a key that can never pass an authz.Require check.
func IsKnownPermission(p Permission) bool {
	_, ok := state.Load().owners[p]
	return ok
}

// AllPermissions returns a copy of the canonical permission catalog in
// registration order (shared entries first, then modules in registry order).
func AllPermissions() []Permission {
	return slices.Clone(state.Load().order)
}

// AllTenantPermissions returns the tenant-scoped permissions in catalog order.
func AllTenantPermissions() []Permission {
	current := state.Load()
	out := make([]Permission, 0, len(current.order))
	for _, p := range current.order {
		if _, platform := current.platform[p]; !platform {
			out = append(out, p)
		}
	}
	return out
}

// AllPlatformPermissions returns the platform-scoped permissions in catalog
// order.
func AllPlatformPermissions() []Permission {
	current := state.Load()
	out := make([]Permission, 0, len(current.platform))
	for _, p := range current.order {
		if _, platform := current.platform[p]; platform {
			out = append(out, p)
		}
	}
	return out
}

// IsPlatformPermission reports whether p is platform-scoped.
func IsPlatformPermission(p Permission) bool {
	_, ok := state.Load().platform[p]
	return ok
}

// OwnerOf returns the owner that registered p ("platform" for the shared
// catalog) and whether p is known.
func OwnerOf(p Permission) (string, bool) {
	owner, ok := state.Load().owners[p]
	return owner, ok
}
