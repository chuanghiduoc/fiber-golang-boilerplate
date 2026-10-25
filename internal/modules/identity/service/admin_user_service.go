package service

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// AdminUserService owns the identity side of the back-office member surface
// of the request tenant: listing memberships (every state), role changes,
// bans, unbans, and the identity half of the dashboard statistics. Every
// operation changes a membership, never the global user.
type AdminUserService interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error)
	UpdateRole(ctx context.Context, id int64, role string) (*dto.UserResponse, error)
	BanUser(ctx context.Context, id int64) error
	UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error)
	GetUserStats(ctx context.Context) (UserStats, error)
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

func (s *adminUserService) ListUsers(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error) {
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

func (s *adminUserService) UpdateRole(ctx context.Context, id int64, role string) (*dto.UserResponse, error) {
	if !ValidRole(role) {
		return nil, apperror.NewBadRequest("role must be one of: user, admin")
	}

	var member *Member
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
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
	return ToUserResponse(member), nil
}

// changeRole sets the member's role, ends their sessions in this tenant
// (their tokens carry the old role), and returns the previous role. The
// final admin is never demoted; that would lock every administrator out.
func changeRole(ctx context.Context, st Stores, id int64, role string) (*Member, string, error) {
	existing, err := getActiveMemberForAdmin(ctx, st.Memberships, id, "user not found")
	if err != nil {
		return nil, "", err
	}
	previous := existing.Membership.Role
	if previous == RoleAdmin && role != RoleAdmin {
		if err := ensureNotLastAdmin(ctx, st.Memberships, "cannot demote the last active admin"); err != nil {
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

func (s *adminUserService) BanUser(ctx context.Context, id int64) error {
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
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
// sessions in this tenant. The last active admin can never be banned.
func banMember(ctx context.Context, st Stores, id int64) error {
	const notFound = "user not found or already banned"
	existing, err := getActiveMemberForAdmin(ctx, st.Memberships, id, notFound)
	if err != nil {
		return err
	}
	if existing.Membership.Role == RoleAdmin {
		if err := ensureNotLastAdmin(ctx, st.Memberships, "cannot ban the last active admin"); err != nil {
			return err
		}
	}
	if _, err := st.Memberships.Ban(ctx, id); err != nil {
		return userWriteError(err, notFound, "failed to ban user")
	}
	if err := st.Sessions.RevokeAllInTenant(ctx, id, RevokeMembershipChanged); err != nil {
		return apperror.NewInternal("failed to revoke sessions")
	}
	return nil
}

func (s *adminUserService) UnbanUser(ctx context.Context, id int64) (*dto.UserResponse, error) {
	var member *Member
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
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
	return ToUserResponse(member), nil
}

// getActiveMemberForAdmin loads an active member through memberships
// (usually the admin mutation's transaction) and reports a missing one with
// notFoundDetail.
func getActiveMemberForAdmin(ctx context.Context, memberships MembershipRepository, id int64, notFoundDetail string) (*Member, error) {
	member, err := memberships.GetActive(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound(notFoundDetail)
		}
		return nil, apperror.NewInternal("failed to get user")
	}
	return member, nil
}

// ensureNotLastAdmin returns 409 user.last_admin when removing one admin
// would leave the request tenant without any. It first takes the per-tenant
// admin-change lock, so concurrent demote/ban/remove requests run one at a
// time and the count below sees the previous one's committed result (READ
// COMMITTED takes a fresh snapshot per statement). Without that lock the
// protect_last_admin trigger would still keep one admin, but two concurrent
// requests can deadlock on each other's rows instead of getting a clean 409.
// The trigger remains the database-level guarantee for other writers.
func ensureNotLastAdmin(ctx context.Context, memberships MembershipRepository, detail string) error {
	if err := memberships.LockAdminChanges(ctx); err != nil {
		return apperror.NewInternal("failed to lock admin changes")
	}
	admins, err := memberships.CountActiveAdmins(ctx)
	if err != nil {
		return apperror.NewInternal("failed to count admins")
	}
	if admins <= 1 {
		return apperror.NewCode(apperror.CodeUserLastAdmin, detail)
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
