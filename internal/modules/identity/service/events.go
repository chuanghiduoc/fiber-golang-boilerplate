package service

import (
	"context"
	"log/slog"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// publishRegisteredTx publishes user.registered through the registration
// transaction, so the account never exists without its event (or vice versa)
// and every durable subscriber (webhooks) sees it.
func publishRegisteredTx(ctx context.Context, publisher EventPublisher, member *Member) error {
	event, err := userRegisteredEvent(member)
	if err == nil {
		err = publisher.Publish(ctx, event)
	}
	if err != nil {
		slog.Error("failed to publish user.registered",
			slog.Int64("user_id", member.User.ID),
			slog.Any("error", err),
		)
		return apperror.NewInternal("failed to create user")
	}
	return nil
}

// userRegisteredEvent builds the user.registered envelope. The user is
// global (ADR 0004): the payload carries only global identity fields, and the
// tenant the account registered into is the envelope's tenantId (the new
// membership's tenant). The new user is the actor, matching the
// auth.registered audit row.
func userRegisteredEvent(member *Member) (events.Event, error) {
	user := member.User
	event, err := events.NewEvent(identitycontracts.EventUserRegistered, identitycontracts.UserRegisteredVersion, identitycontracts.UserRegistered{
		UserID:        user.ID,
		Email:         user.Email,
		Name:          user.Name,
		EmailVerified: user.EmailVerified(),
		CreatedAt:     user.CreatedAt.UTC(),
	})
	if err != nil {
		return events.Event{}, err
	}
	event.TenantID = member.Membership.TenantID
	event.Actor = events.UserActor(user.ID)
	return event, nil
}
