// Package repository adapts the identity and files admin contracts to the
// ports the admin service declares. The admin module owns no tables.
package repository

import (
	"context"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	adminsvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
)

// IdentityAdmin maps the identity contract onto the admin service port.
type IdentityAdmin struct {
	contracts identitycontracts.IdentityAdmin
}

var _ adminsvc.IdentityAdmin = (*IdentityAdmin)(nil)

func NewIdentityAdmin(contracts identitycontracts.IdentityAdmin) *IdentityAdmin {
	return &IdentityAdmin{contracts: contracts}
}

func (r *IdentityAdmin) ListUsers(ctx context.Context, limit int, startingAfter string) ([]adminsvc.User, bool, error) {
	page, err := r.contracts.ListUsers(ctx, limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	users := make([]adminsvc.User, len(page.Items))
	for i := range page.Items {
		users[i] = toUser(page.Items[i])
	}
	return users, page.HasMore, nil
}

func (r *IdentityAdmin) UpdateRole(ctx context.Context, id int64, role string) (*adminsvc.User, error) {
	user, err := r.contracts.UpdateRole(ctx, id, role)
	if err != nil {
		return nil, err
	}
	return toUserPtr(user), nil
}

func (r *IdentityAdmin) BanUser(ctx context.Context, id int64) error {
	return r.contracts.BanUser(ctx, id)
}

func (r *IdentityAdmin) UnbanUser(ctx context.Context, id int64) (*adminsvc.User, error) {
	user, err := r.contracts.UnbanUser(ctx, id)
	if err != nil {
		return nil, err
	}
	return toUserPtr(user), nil
}

func (r *IdentityAdmin) GetUserStats(ctx context.Context) (adminsvc.UserStats, error) {
	stats, err := r.contracts.GetUserStats(ctx)
	if err != nil {
		return adminsvc.UserStats{}, err
	}
	return adminsvc.UserStats{
		ActiveUsers:  stats.ActiveUsers,
		DeletedUsers: stats.DeletedUsers,
	}, nil
}

func toUser(user identitycontracts.User) adminsvc.User {
	return adminsvc.User{
		ID:               user.ID,
		Email:            user.Email,
		Name:             user.Name,
		Role:             user.Role,
		MembershipStatus: user.MembershipStatus,
		EmailVerified:    user.EmailVerified,
		HasPassword:      user.HasPassword,
		CreatedAt:        user.CreatedAt,
		UpdatedAt:        user.UpdatedAt,
		JoinedAt:         user.JoinedAt,
		DeletedAt:        user.DeletedAt,
		BannedAt:         user.BannedAt,
	}
}

func toUserPtr(user identitycontracts.User) *adminsvc.User {
	mapped := toUser(user)
	return &mapped
}
