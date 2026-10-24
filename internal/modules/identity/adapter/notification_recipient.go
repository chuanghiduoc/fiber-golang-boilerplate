package adapter

import (
	"context"
	"errors"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// NotificationRecipient adapts the tenant-scoped user repository to the
// notifications module's recipient lookup. Identity remains the owner of user
// email addresses; notifications never query the users table.
type NotificationRecipient struct {
	users repository.UserRepository
}

var _ contracts.NotificationRecipientLookup = (*NotificationRecipient)(nil)

func NewNotificationRecipient(users repository.UserRepository) *NotificationRecipient {
	return &NotificationRecipient{users: users}
}

func (a *NotificationRecipient) NotificationRecipient(ctx context.Context, userID int64) (contracts.NotificationRecipient, error) {
	user, err := a.users.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return contracts.NotificationRecipient{}, apperror.ErrNotFound
		}
		return contracts.NotificationRecipient{}, err
	}
	return contracts.NotificationRecipient{
		UserID: user.ID,
		Email:  user.Email,
		Name:   user.Name,
	}, nil
}
