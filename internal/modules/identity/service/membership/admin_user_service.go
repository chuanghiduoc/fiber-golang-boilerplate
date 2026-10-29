package membership

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// AdminUserService owns the identity side of the back-office member surface
// of the request tenant: listing memberships (every state), role changes,
// bans, unbans, and the identity half of the dashboard statistics. Every
// operation changes a membership, never the global user.
type AdminUserService interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]Member, bool, error)
	ListMembers(ctx context.Context, limit int, startingAfter, status, role string) ([]Member, bool, error)
	GetMember(ctx context.Context, id uuid.UUID) (*Member, error)
	UpdateMember(ctx context.Context, id uuid.UUID, update MemberUpdate) (*Member, error)
	UpdateRole(ctx context.Context, id uuid.UUID, role string) (*Member, error)
	BanUser(ctx context.Context, id uuid.UUID) error
	UnbanUser(ctx context.Context, id uuid.UUID) (*Member, error)
	GetUserStats(ctx context.Context) (UserStats, error)
}

// MemberUpdate is the merge-patched member state from PATCH /members/{id}.
// ExpectedUpdatedAt is the validator the caller read; a mismatch is 412.
type MemberUpdate struct {
	ExpectedUpdatedAt time.Time
	RoleKeys          []string
	Status            *string
}

// UserStats is the identity-owned aggregation consumed by the admin module.
type UserStats struct {
	ActiveUsers  int64
	DeletedUsers int64
}

type adminUserService struct {
	stores       Stores
	tx           Transactor
	sessionCache session.Store
}

// NewAdminUserService wires the admin member service. sessionCache may be nil.
func NewAdminUserService(stores Stores, tx Transactor, sessionCache session.Store) AdminUserService {
	return &adminUserService{stores: stores, tx: tx, sessionCache: sessionCache}
}

func (s *adminUserService) ListUsers(ctx context.Context, limit int, startingAfter string) (users []Member, hasMore bool, err error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	members, err := s.stores.Memberships.ListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list users")
	}
	return memberPage(members, pageSize)
}

func (s *adminUserService) ListMembers(ctx context.Context, limit int, startingAfter, status, role string) (listed []Member, hasMore bool, err error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	status = strings.TrimSpace(status)
	role = strings.TrimSpace(role)
	if status != "" && !ValidMembershipStatus(status) {
		return nil, false, apperror.NewBadRequest("status must be active, suspended, or removed")
	}
	if role != "" {

		if !roleKeyPattern.MatchString(role) {
			return nil, false, apperror.NewBadRequest("invalid role")
		}
	}
	members, err := s.stores.Memberships.ListFilteredCursor(ctx, cur, status, role)
	if err != nil {
		return nil, false, apperror.Internal(err, "failed to list users")
	}
	return memberPage(members, pageSize)
}

// GetMember returns one membership of the request tenant in any state.
func (s *adminUserService) GetMember(ctx context.Context, id uuid.UUID) (*Member, error) {
	member, err := s.stores.Memberships.Get(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.Internal(err, "failed to load user")
	}
	return member, nil
}

// UpdateMember applies a merge-patched role and/or membership status change in
// one transaction. The last-owner guard, rank checks, and session revocation
// are shared with the single-purpose admin mutations.
func (s *adminUserService) UpdateMember(ctx context.Context, id uuid.UUID, update MemberUpdate) (*Member, error) {
	if err := authorizeMemberUpdate(ctx, update); err != nil {
		return nil, err
	}
	var member *Member
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		current, err := loadMemberForAdminMutation(ctx, tx.Memberships, id)
		if err != nil {
			return err
		}
		// Roles and status share the same hierarchy rule: changing either
		// requires strictly higher rank. The role path checks this later in
		// replaceMemberRolesTx, but a status-only patch never reaches it, so
		// enforce it once here before any write (including the ETag CAS).
		if err := EnsureCanManageMember(ctx, tx.Stores, id); err != nil {
			return err
		}
		// The conditional UPDATE is the compare-and-swap: two writers that
		// read the same ETag cannot both pass, and the loser gets 412 without
		// touching roles, status, or the audit trail.
		if !update.ExpectedUpdatedAt.IsZero() {
			won, err := tx.Memberships.TouchIfUnchanged(ctx, id, update.ExpectedUpdatedAt)
			if err != nil {
				return apperror.Internal(err, "failed to update user")
			}
			if !won {
				return apperror.NewPreconditionFailed("the member has changed since it was read; refresh and retry")
			}
		}
		if current.Membership.Status == MembershipStatusRemoved {
			return apperror.NewNotFound("user not found")
		}
		if err := applyMemberRoles(ctx, tx, current, id, update.RoleKeys); err != nil {
			return err
		}
		if err := applyMemberStatus(ctx, tx, current, id, update.Status); err != nil {
			return err
		}
		updated, err := loadMemberForAdminMutation(ctx, tx.Memberships, id)
		if err != nil {
			return err
		}
		member = updated
		metadata := map[string]any{}
		if len(update.RoleKeys) > 0 {
			metadata["roles"] = update.RoleKeys
		}
		if update.Status != nil {
			metadata["status"] = *update.Status
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "admin.user.updated", id, metadata)))
	})
	if err != nil {
		return nil, err
	}
	invalidateSessions(ctx, s.sessionCache, id)
	return member, nil
}

// authorizeMemberUpdate enforces the capability for every field present in
// the merge patch. The route accepts AnyOf(role, ban, delete), but a role
// manager must not be able to suspend/remove a member and a ban manager must
// not be able to change roles. Removal additionally requires the dedicated
// delete grant.
func authorizeMemberUpdate(ctx context.Context, update MemberUpdate) error {
	// The handler rejects a null or empty roleIds, so a non-empty slice is the
	// service-level representation of a present role field.
	if len(update.RoleKeys) > 0 {
		if err := requirePermission(ctx, authz.PermissionAdminUsersRole); err != nil {
			return err
		}
	}
	if update.Status == nil {
		return nil
	}
	switch MembershipStatus(strings.TrimSpace(*update.Status)) {
	case MembershipStatusActive, MembershipStatusSuspended:
		return requirePermission(ctx, authz.PermissionAdminUsersBan)
	case MembershipStatusRemoved:
		return requirePermission(ctx, authz.PermissionAdminUsersDelete)
	default:
		// applyMemberStatus returns the validation error after all permissions
		// for the other fields have also had a chance to fail closed.
		return nil
	}
}

func loadMemberForAdminMutation(ctx context.Context, memberships MembershipRepository, id uuid.UUID) (*Member, error) {
	member, err := memberships.Get(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.Internal(err, "failed to load user")
	}
	return member, nil
}

func applyMemberRoles(ctx context.Context, tx TxRepos, current *Member, id uuid.UUID, roleKeys []string) error {
	if len(roleKeys) == 0 {
		return nil
	}
	if current.Membership.Status != MembershipStatusActive {
		return apperror.NewBadRequest("roles can only be changed for an active member")
	}
	roles, err := normalizeRoleKeys(roleKeys)
	if err != nil {
		return err
	}
	_, err = replaceMemberRolesTx(ctx, tx, id, roles)
	return err
}

func applyMemberStatus(ctx context.Context, tx TxRepos, current *Member, id uuid.UUID, raw *string) error {
	if raw == nil {
		return nil
	}
	status := MembershipStatus(strings.TrimSpace(*raw))
	if !ValidMembershipStatus(string(status)) {
		return apperror.NewBadRequest("status must be active, suspended, or removed")
	}
	switch status {
	case MembershipStatusActive:
		return reactivateMember(ctx, tx, current, id)
	case MembershipStatusSuspended:
		return suspendMember(ctx, tx, current, id)
	case MembershipStatusRemoved:
		return RemoveMembership(ctx, tx.Stores, id)
	}
	return nil
}

func reactivateMember(ctx context.Context, tx TxRepos, current *Member, id uuid.UUID) error {
	if current.Membership.Status != MembershipStatusSuspended {
		return nil
	}
	if _, err := tx.Memberships.Unban(ctx, id); err != nil {
		return userWriteError(err, "user not found or not banned", "failed to unban user")
	}
	return nil
}

func suspendMember(ctx context.Context, tx TxRepos, current *Member, id uuid.UUID) error {
	if current.Membership.Status != MembershipStatusActive {
		return nil
	}
	return banMember(ctx, tx.Stores, id)
}

func (s *adminUserService) UpdateRole(ctx context.Context, id uuid.UUID, role string) (*Member, error) {
	if !ValidRole(role) {
		return nil, apperror.NewBadRequest("role must be one of: member, admin, owner")
	}

	var member *Member
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := EnsureCanManageMember(ctx, tx.Stores, id); err != nil {
			return err
		}
		if err := EnsureCanGrantRoles(ctx, tx.Stores, []string{role}); err != nil {
			return err
		}
		updated, previousRole, updateErr := changeRole(ctx, tx.Stores, id, role)
		if updateErr != nil {
			return updateErr
		}
		member = updated
		event := userEvent(ctx, "admin.user.role_changed", id, map[string]any{"from_role": previousRole, "to_role": role})
		return checkAudit(tx.Audit.RecordTx(ctx, event))
	})
	if err != nil {
		return nil, err
	}

	invalidateSessions(ctx, s.sessionCache, id)
	return member, nil
}

// changeRole replaces the member's roles with role, ends their sessions in
// this tenant (their snapshots carry the old permission set), and returns the
// previous highest role. The final owner is never demoted; that would leave
// the tenant without an owner.
func changeRole(ctx context.Context, st Stores, id uuid.UUID, role string) (member *Member, oldRole string, err error) {
	existing, err := getActiveMemberForAdmin(ctx, st.Memberships, id, "user not found")
	if err != nil {
		return nil, "", err
	}
	previous := existing.Membership.Role
	if previous == RoleOwner && role != RoleOwner {
		if err := ensureNotLastOwner(ctx, st.Memberships, "cannot demote the last active owner"); err != nil {
			return nil, "", err
		}
	}
	membership, err := st.Memberships.UpdateRole(ctx, id, role)
	if err != nil {
		return nil, "", userWriteError(err, "user not found", "failed to update user role")
	}
	if previous != role {
		if err := st.Sessions.RevokeAllInTenant(ctx, id, RevokeMembershipChanged); err != nil {
			return nil, "", apperror.NewInternal("failed to revoke sessions")
		}
	}
	existing.Membership = *membership
	return existing, previous, nil
}

func (s *adminUserService) BanUser(ctx context.Context, id uuid.UUID) error {
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := EnsureCanManageMember(ctx, tx.Stores, id); err != nil {
			return err
		}
		if banErr := banMember(ctx, tx.Stores, id); banErr != nil {
			return banErr
		}
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "admin.user.banned", id, nil)))
	})
	if err != nil {
		return err
	}
	invalidateSessions(ctx, s.sessionCache, id)
	return nil
}

// banMember suspends the membership (which also revokes the member's API
// keys in this tenant through credentials_revoked_at) and ends their
// sessions in this tenant. The last active owner can never be banned.
func banMember(ctx context.Context, st Stores, id uuid.UUID) error {
	const notFound = "user not found or already banned"
	existing, err := getActiveMemberForAdmin(ctx, st.Memberships, id, notFound)
	if err != nil {
		return err
	}
	if existing.Membership.Role == RoleOwner {
		if err := ensureNotLastOwner(ctx, st.Memberships, "cannot ban the last active owner"); err != nil {
			return err
		}
	}
	marker, err := newBanMarker(st.Bans, existing.User.Email)
	if err != nil {
		return err
	}
	if _, err := st.Memberships.Ban(ctx, id, marker); err != nil {
		return userWriteError(err, notFound, "failed to ban user")
	}
	if err := st.Sessions.RevokeAllInTenant(ctx, id, RevokeMembershipChanged); err != nil {
		return apperror.NewInternal("failed to revoke sessions")
	}
	return nil
}

func (s *adminUserService) UnbanUser(ctx context.Context, id uuid.UUID) (*Member, error) {
	var member *Member
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		if err := EnsureCanManageMember(ctx, tx.Stores, id); err != nil {
			return err
		}
		if _, err := tx.Memberships.Unban(ctx, id); err != nil {
			return userWriteError(err, "user not found or not banned", "failed to unban user")
		}
		restored, err := tx.Memberships.Get(ctx, id)
		if err != nil {
			return userWriteError(err, "user not found or not banned", "failed to unban user")
		}
		member = restored
		return checkAudit(tx.Audit.RecordTx(ctx, userEvent(ctx, "admin.user.unbanned", id, nil)))
	})
	if err != nil {
		return nil, err
	}
	invalidateSessions(ctx, s.sessionCache, id)
	return member, nil
}

// getActiveMemberForAdmin loads an active member through memberships
// (usually the admin mutation's transaction) and reports a missing one with
// notFoundDetail.
func getActiveMemberForAdmin(ctx context.Context, memberships MembershipRepository, id uuid.UUID, notFoundDetail string) (*Member, error) {
	member, err := memberships.GetActive(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound(notFoundDetail)
		}
		return nil, apperror.NewInternal("failed to get user")
	}
	return member, nil
}

// RemoveMembership removes the caller-managed membership, preserving a ban marker.
func RemoveMembership(ctx context.Context, st Stores, id uuid.UUID) error {
	const notFound = "user not found"
	if err := EnsureCanManageMember(ctx, st, id); err != nil {
		return err
	}
	member, err := st.Memberships.Get(ctx, id)
	if err != nil {
		return userWriteError(err, notFound, "failed to get user")
	}
	if member.Membership.Status == MembershipStatusRemoved {
		return apperror.NewNotFound(notFound)
	}
	if member.Membership.Role == RoleOwner && member.Membership.Status == MembershipStatusActive {
		if err := ensureNotLastOwner(ctx, st.Memberships, "cannot delete the last active owner"); err != nil {
			return err
		}
	}
	marker, err := removalBanMarker(st.Bans, &member.Membership, member.User.Email)
	if err != nil {
		return err
	}
	if _, err := st.Memberships.Remove(ctx, id, marker); err != nil {
		return userWriteError(err, notFound, "failed to remove membership")
	}
	if err := st.Sessions.RevokeAllInTenant(ctx, id, RevokeMembershipChanged); err != nil {
		return apperror.NewInternal("failed to revoke sessions")
	}
	return nil
}

func (s *adminUserService) GetUserStats(ctx context.Context) (UserStats, error) {
	stats, err := s.stores.Memberships.Stats(ctx)
	if err != nil {
		return UserStats{}, apperror.NewInternal("failed to get user stats")
	}
	return UserStats{ActiveUsers: stats.Active, DeletedUsers: stats.Inactive}, nil
}
