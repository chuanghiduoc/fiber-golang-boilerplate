package adapter

import (
	"context"
	"errors"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// NotificationRecipient adapts identity memberships to the notifications
// module's recipient lookup. Identity remains the owner of user email
// addresses; notifications never query the users table. Only an active member
// of the context tenant is a recipient.
type NotificationRecipient struct {
	memberships identityservice.MembershipRepository
}

var _ identitycontracts.NotificationRecipientLookup = (*NotificationRecipient)(nil)

func NewNotificationRecipient(memberships identityservice.MembershipRepository) *NotificationRecipient {
	return &NotificationRecipient{memberships: memberships}
}

func (a *NotificationRecipient) NotificationRecipient(ctx context.Context, userID uuid.UUID) (identitycontracts.NotificationRecipient, error) {
	member, err := a.memberships.GetActive(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return identitycontracts.NotificationRecipient{}, apperror.ErrNotFound
		}
		return identitycontracts.NotificationRecipient{}, err
	}
	return identitycontracts.NotificationRecipient{
		UserID: member.User.ID,
		Email:  member.User.Email,
		Name:   member.User.Name,
	}, nil
}
