// Package authz defines the authorization capability catalog, the mapping from
// roles to permissions, and Fiber middleware that enforces permissions on
// routes. It has no dependency on the transport DTOs so modules can share the
// catalog without importing handler-level types.
//
// The catalog is an injected instance (authz.Catalog): the composition root
// builds it from the shared platform grants plus every app.PermissionProvider,
// detects duplicate or conflicting registrations, seals it, and injects it
// into the session/permission sync, the services' authz.Authorizer, role
// validation, and the API-key scope intersection. There is deliberately no
// process-wide mutable registry.
package authz

import (
	"fmt"
	"regexp"
	"slices"
	"sync"
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
	PermissionPlatformFeaturesRead  Permission = "platform:features:read"
	PermissionPlatformFeaturesWrite Permission = "platform:features:write"
	PermissionPlatformUsersMFAReset Permission = "platform:users:mfa_reset"
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

// Provider contributes one owner's grants to a Catalog. The composition root
// builds one provider per app.PermissionProvider plus the shared platform
// grants.
type Provider struct {
	Owner  string
	Grants []Grant
}

// permissionPattern keeps identifiers stable and scope-friendly:
// lower-case segments separated by ':' (for example "orders:read" or
// "admin:api_keys:rotate").
var permissionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(:[a-z][a-z0-9_]*)+$`)

// Catalog is an injected, instance-scoped permission catalog. It is populated
// at the composition root (or by a test helper) and sealed before serving, so
// the per-request read path is lock-free after startup.
type Catalog struct {
	mu       sync.RWMutex
	order    []Permission
	owners   map[Permission]string
	grants   map[Permission][]string
	platform map[Permission]struct{}
	roles    map[string]map[Permission]struct{}
	sealed   bool
}

// NewCatalog returns a catalog pre-populated with the shared platform grants.
// It is unsealed so the composition root can add module providers.
func NewCatalog() *Catalog {
	c := &Catalog{
		owners:   map[Permission]string{},
		grants:   map[Permission][]string{},
		platform: map[Permission]struct{}{},
		roles: map[string]map[Permission]struct{}{
			RoleMember:        {},
			RoleAdmin:         {},
			RoleOwner:         {},
			RolePlatformAdmin: {},
		},
	}
	// Shared grants are trusted constants; a failure here is a build bug.
	if err := c.Add(Provider{Owner: platformOwner, Grants: sharedGrants}); err != nil {
		panic(err)
	}
	return c
}

var sharedGrants = []Grant{
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
	{Permission: PermissionPlatformFeaturesRead, Platform: true},
	{Permission: PermissionPlatformFeaturesWrite, Platform: true},
	{Permission: PermissionPlatformUsersMFAReset, Platform: true},
}

// Add contributes owner's grants. It fails on an invalid permission
// identifier, an unknown role, a permission listed twice, a permission already
// registered by a different owner or with different roles, and on a sealed
// catalog. Re-adding the identical grants for the same owner is a no-op. A
// failed call adds nothing.
func (c *Catalog) Add(provider Provider) error {
	if provider.Owner == "" {
		return fmt.Errorf("authz: add: owner is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealed {
		return fmt.Errorf("authz: %s: catalog is sealed", provider.Owner)
	}

	type pendingGrant struct {
		permission Permission
		roles      []string
		platform   bool
	}
	pending := make([]pendingGrant, 0, len(provider.Grants))
	seen := make(map[Permission]struct{}, len(provider.Grants))
	for _, grant := range provider.Grants {
		p := grant.Permission
		if !permissionPattern.MatchString(string(p)) {
			return fmt.Errorf("authz: %s: invalid permission %q (want lower-case segments joined by ':')", provider.Owner, p)
		}
		if _, dup := seen[p]; dup {
			return fmt.Errorf("authz: %s: permission %q listed twice", provider.Owner, p)
		}
		seen[p] = struct{}{}

		roles, err := normalizeRoles(grant)
		if err != nil {
			return fmt.Errorf("authz: %s: permission %q: %w", provider.Owner, p, err)
		}
		if existingOwner, exists := c.owners[p]; exists {
			if existingOwner != provider.Owner {
				return fmt.Errorf("authz: permission %q registered by %q is already owned by %q", p, provider.Owner, existingOwner)
			}
			_, wasPlatform := c.platform[p]
			if !slices.Equal(c.grants[p], roles) || wasPlatform != grant.Platform {
				return fmt.Errorf("authz: %s: permission %q re-registered with roles %v (was %v)", provider.Owner, p, roles, c.grants[p])
			}
			continue
		}
		pending = append(pending, pendingGrant{permission: p, roles: roles, platform: grant.Platform})
	}
	for _, grant := range pending {
		c.order = append(c.order, grant.permission)
		c.owners[grant.permission] = provider.Owner
		c.grants[grant.permission] = grant.roles
		if grant.platform {
			c.platform[grant.permission] = struct{}{}
			c.roles[RolePlatformAdmin][grant.permission] = struct{}{}
		} else {
			c.roles[RoleAdmin][grant.permission] = struct{}{}
			c.roles[RoleOwner][grant.permission] = struct{}{}
			for _, role := range grant.roles {
				c.roles[role][grant.permission] = struct{}{}
			}
		}
	}
	return nil
}

// Seal prevents further additions.
func (c *Catalog) Seal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sealed = true
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

// PermissionsForRole returns a copy of the permission set granted to role.
// Unknown roles yield an empty, non-nil map. Callers may mutate the result
// without affecting the catalog.
func (c *Catalog) PermissionsForRole(role string) map[Permission]struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	granted := c.roles[role]
	out := make(map[Permission]struct{}, len(granted))
	for p := range granted {
		out[p] = struct{}{}
	}
	return out
}

// Has reports whether role is granted permission p.
func (c *Catalog) Has(role string, p Permission) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.roles[role][p]
	return ok
}

// IsKnownPermission reports whether p is part of the published catalog. It is
// used by API-key creation to reject unknown scopes instead of silently
// creating a key that can never pass an authz.Require check.
func (c *Catalog) IsKnownPermission(p Permission) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.owners[p]
	return ok
}

// AllPermissions returns a copy of the canonical permission catalog in
// registration order (shared entries first, then modules in registry order).
func (c *Catalog) AllPermissions() []Permission {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.order)
}

// AllTenantPermissions returns the tenant-scoped permissions in catalog order.
func (c *Catalog) AllTenantPermissions() []Permission {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Permission, 0, len(c.order))
	for _, p := range c.order {
		if _, platform := c.platform[p]; !platform {
			out = append(out, p)
		}
	}
	return out
}

// AllPlatformPermissions returns the platform-scoped permissions in catalog
// order.
func (c *Catalog) AllPlatformPermissions() []Permission {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Permission, 0, len(c.platform))
	for _, p := range c.order {
		if _, platform := c.platform[p]; platform {
			out = append(out, p)
		}
	}
	return out
}

// IsPlatformPermission reports whether p is platform-scoped.
func (c *Catalog) IsPlatformPermission(p Permission) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.platform[p]
	return ok
}

// OwnerOf returns the owner that registered p ("platform" for the shared
// catalog) and whether p is known.
func (c *Catalog) OwnerOf(p Permission) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	owner, ok := c.owners[p]
	return owner, ok
}
