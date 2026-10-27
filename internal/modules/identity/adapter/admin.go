// Package adapter exposes the identity module's implementation of the
// cross-module admin contracts. It translates the module's DTOs into
// transport-neutral contract types so no caller imports identity internals.
package adapter

import (
	"context"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/google/uuid"
)

// AdminUsers is the admin-user surface this adapter consumes.
// *membership.AdminUserService implements it.
type AdminUsers interface {
	ListUsers(ctx context.Context, limit int, startingAfter string) ([]core.Member, bool, error)
	UpdateRole(ctx context.Context, id uuid.UUID, role string) (*core.Member, error)
	BanUser(ctx context.Context, id uuid.UUID) error
	UnbanUser(ctx context.Context, id uuid.UUID) (*core.Member, error)
	GetUserStats(ctx context.Context) (membership.UserStats, error)
}

// AdminIdentity adapts the identity admin service to identitycontracts.IdentityAdmin.
type AdminIdentity struct {
	service AdminUsers
}

var _ identitycontracts.IdentityAdmin = (*AdminIdentity)(nil)

func NewAdminIdentity(svc AdminUsers) *AdminIdentity {
	return &AdminIdentity{service: svc}
}

func (a *AdminIdentity) ListUsers(ctx context.Context, limit int, startingAfter string) (identitycontracts.UserPage, error) {
	users, hasMore, err := a.service.ListUsers(ctx, limit, startingAfter)
	if err != nil {
		return identitycontracts.UserPage{}, err
	}
	items := make([]identitycontracts.User, len(users))
	for i := range users {
		items[i] = toUser(&users[i])
	}
	return identitycontracts.UserPage{Items: items, HasMore: hasMore}, nil
}

func (a *AdminIdentity) UpdateRole(ctx context.Context, id uuid.UUID, role string) (identitycontracts.User, error) {
	user, err := a.service.UpdateRole(ctx, id, role)
	if err != nil {
		return identitycontracts.User{}, err
	}
	return toUser(user), nil
}

func (a *AdminIdentity) BanUser(ctx context.Context, id uuid.UUID) error {
	return a.service.BanUser(ctx, id)
}

func (a *AdminIdentity) UnbanUser(ctx context.Context, id uuid.UUID) (identitycontracts.User, error) {
	user, err := a.service.UnbanUser(ctx, id)
	if err != nil {
		return identitycontracts.User{}, err
	}
	return toUser(user), nil
}

func (a *AdminIdentity) GetUserStats(ctx context.Context) (identitycontracts.UserStats, error) {
	stats, err := a.service.GetUserStats(ctx)
	if err != nil {
		return identitycontracts.UserStats{}, err
	}
	return identitycontracts.UserStats{
		ActiveUsers:  stats.ActiveUsers,
		DeletedUsers: stats.DeletedUsers,
	}, nil
}

func toUser(member *core.Member) identitycontracts.User {
	user, mem := member.User, member.Membership
	return identitycontracts.User{
		ID:               user.ID,
		Email:            user.Email,
		Name:             user.Name,
		Role:             mem.Role,
		Roles:            mem.Roles,
		MembershipStatus: string(mem.Status),
		EmailVerified:    user.EmailVerified(),
		HasPassword:      user.HasPassword,
		CreatedAt:        user.CreatedAt,
		UpdatedAt:        member.UpdatedAt(),
		JoinedAt:         mem.CreatedAt,
		DeletedAt:        member.DeletedAt(),
		BannedAt:         mem.BannedAt,
	}
}
