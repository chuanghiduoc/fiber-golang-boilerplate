package service

import (
	"context"
	"errors"
	"slices"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Tenant role ranks. Owner outranks admin, which outranks every other role
// (member and tenant-defined roles share the lowest rank). The rank is not
// carried by the JWT: helpers read the caller's membership_roles in the
// request tenant, inside the mutation transaction.
const (
	roleRankOwner  = 3
	roleRankAdmin  = 2
	roleRankMember = 1
)

func roleRank(role string) int {
	switch role {
	case authz.RoleOwner:
		return roleRankOwner
	case authz.RoleAdmin:
		return roleRankAdmin
	default:
		return roleRankMember
	}
}

// highestRoleRank returns the highest rank among roles; a membership without
// roles is treated as member, like the compatibility role derived by queries.
func highestRoleRank(roles []string) int {
	best := 0
	for _, role := range roles {
		if rank := roleRank(role); rank > best {
			best = rank
		}
	}
	if best == 0 {
		return roleRankMember
	}
	return best
}

// canManageRank reports whether a caller of callerRank may change or remove a
// member of targetRank. Management requires a strictly higher rank, except
// that owners may manage other owners (co-owners).
func canManageRank(callerRank, targetRank int) bool {
	return callerRank > targetRank || callerRank == roleRankOwner
}

// canGrantRank reports whether a caller of callerRank may grant a role of
// roleRank. Only an owner can grant owner; an admin can grant admin, member,
// and tenant roles; every other caller is limited to the lowest rank.
func canGrantRank(callerRank, roleRank int) bool {
	return callerRank >= roleRank
}

// EnsureCanManageMember checks that the caller may change the target
// membership (role change, demotion, ban, unban, removal) in the request
// tenant. It must run inside the mutation transaction: it locks the target
// membership row FOR UPDATE and compares against the target's highest current
// role. The membership row is always taken before the per-tenant owner-change
// advisory lock (see ensureNotLastOwner), so concurrent mutations share one
// lock order. A caller without a subject is allowed only under
// authz.SystemContext.
func EnsureCanManageMember(ctx context.Context, stores Stores, targetUserID int64) error {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok {
		if authz.IsSystemContext(ctx) {
			return nil
		}
		return permissionDenied()
	}
	caller, err := stores.Memberships.GetActive(ctx, subject.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return permissionDenied()
		}
		return apperror.NewInternal("failed to load caller membership")
	}
	if err := stores.Memberships.LockMembership(ctx, targetUserID); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to lock membership")
	}
	target, err := stores.Memberships.Get(ctx, targetUserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to load target membership")
	}
	if !canManageRank(highestRoleRank(caller.Membership.Roles), highestRoleRank(target.Membership.Roles)) {
		return insufficientRank("cannot manage a member with an equal or higher role")
	}
	return nil
}

// EnsureCanGrantRoles checks that the caller may assign roleKeys in the
// request tenant: the caller's rank must be at least every role's rank and
// the caller must hold every permission the roles grant. A caller without a
// subject is allowed only under authz.SystemContext.
func EnsureCanGrantRoles(ctx context.Context, stores Stores, roleKeys []string) error {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok {
		if authz.IsSystemContext(ctx) {
			return nil
		}
		return permissionDenied()
	}
	caller, err := stores.Memberships.GetActive(ctx, subject.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return permissionDenied()
		}
		return apperror.NewInternal("failed to load caller membership")
	}
	callerRank := highestRoleRank(caller.Membership.Roles)
	for _, key := range roleKeys {
		role, err := stores.Roles.GetByKey(ctx, key)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewBadRequest("unknown role " + key)
			}
			return apperror.NewInternal("failed to load role")
		}
		if !canGrantRank(callerRank, roleRank(role.Key)) {
			return insufficientRank("cannot grant a role with a higher rank")
		}
		if err := authorizeGrant(ctx, role.Permissions); err != nil {
			return err
		}
	}
	return nil
}

// ensureInviterCanGrant re-checks an invitation's inviter inside the accept
// transaction: they must still be an active member of the invitation's tenant
// and still hold the rank and permissions needed to grant its role.
func ensureInviterCanGrant(ctx context.Context, stores Stores, inviterID int64, roleKey string) error {
	const detail = "the inviter can no longer grant this role"
	role, err := stores.Roles.GetByKey(ctx, roleKey)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewCode(identitycatalog.CodeInvitationInviterIneligible, detail)
		}
		return apperror.NewInternal("failed to load invitation role")
	}
	principal, err := stores.Memberships.GetPrincipal(ctx, inviterID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewCode(identitycatalog.CodeInvitationInviterIneligible, detail)
		}
		return apperror.NewInternal("failed to load the inviter membership")
	}
	if !canGrantRank(highestRoleRank([]string{principal.Role}), roleRank(role.Key)) {
		return apperror.NewCode(identitycatalog.CodeInvitationInviterIneligible, detail)
	}
	for _, permission := range role.Permissions {
		if !slices.Contains(principal.Permissions, permission) {
			return apperror.NewCode(identitycatalog.CodeInvitationInviterIneligible, detail)
		}
	}
	return nil
}

func insufficientRank(detail string) error {
	return apperror.NewCode(identitycatalog.CodeRoleInsufficientRank, detail)
}

func permissionDenied() error {
	return apperror.NewCode(apperror.CodePermissionDenied, "insufficient permissions")
}
