package adapter

import (
	"context"
	"errors"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// NotificationRecipient adapts the tenant-scoped user repository to the
// notifications module's recipient lookup. Identity remains the owner of user
// email addresses; notifications never query the users table.
type NotificationRecipient struct {
	users repository.UserRepository
}

var _ identitycontracts.NotificationRecipientLookup = (*NotificationRecipient)(nil)

func NewNotificationRecipient(users repository.UserRepository) *NotificationRecipient {
	return &NotificationRecipient{users: users}
}

func (a *NotificationRecipient) NotificationRecipient(ctx context.Context, userID int64) (identitycontracts.NotificationRecipient, error) {
	user, err := a.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return identitycontracts.NotificationRecipient{}, apperror.ErrNotFound
		}
		return identitycontracts.NotificationRecipient{}, err
	}
	return identitycontracts.NotificationRecipient{
		UserID: user.ID,
		Email:  user.Email,
		Name:   user.Name,
	}, nil
}
