package authn

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// RegisterInput creates an account in the request tenant.
type RegisterInput struct {
	Email    string
	Password string
	Name     string
}

// Register creates a global user with a password and an active membership
// in the request tenant. An address that already has an account gets 409
// user.email_taken even when that account is not a member of this tenant:
// an existing user joins another tenant by accepting an invitation, never by
// registering a second account. The user, membership, optional verification
// email job, auth.registered audit row, and user.registered event commit in
// one transaction: any failure rolls the account back, so a registration
// never exists without its audit record or outbox rows.
func (s *userService) Register(ctx context.Context, req RegisterInput) (*Member, error) {
	email := NormalizeEmail(req.Email)
	name := strings.TrimSpace(req.Name)

	if err := ValidatePasswordForEmail(req.Password, email, "password"); err != nil {
		return nil, err
	}
	if err := ensureEmailUnregistered(ctx, s.Stores, email); err != nil {
		return nil, err
	}
	// Hash before opening a transaction: Argon2id is intentionally expensive and
	// must not hold a pooled database connection for hundreds of milliseconds.
	hash, err := hashPassword(req.Password)
	if err != nil {
		return nil, err
	}

	var member *Member
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		created, createErr := createAccount(ctx, tx.Stores, NewUser{Email: email, Name: name}, hash, RoleMember)
		if createErr != nil {
			return createErr
		}
		if provErr := s.provisionVerificationTx(ctx, tx, &created.User); provErr != nil {
			return provErr
		}
		if auditErr := checkAudit(tx.Audit.RecordTx(ctx, s.registeredEvent(ctx, created.User.ID))); auditErr != nil {
			return auditErr
		}
		if pubErr := publishRegisteredTx(ctx, tx.Events, created); pubErr != nil {
			return pubErr
		}
		member = created
		return nil
	})
	if err != nil {
		return nil, err
	}

	// A previous failed-login lockout must not survive registration.
	clearLoginAttempts(ctx, s.Cache, email)
	return member, nil
}

// ensureEmailUnregistered rejects an address that belongs to a live account
// (in any tenant) or to an account banned in the request tenant.
func ensureEmailUnregistered(ctx context.Context, st Stores, email string) error {
	_, err := st.Users.GetByEmail(ctx, email)
	switch {
	case err == nil:
		return emailTaken()
	case !errors.Is(err, apperror.ErrNotFound):
		return apperror.NewInternal("failed to check existing user")
	}
	return ensureEmailNotBanned(ctx, st, email)
}

// ensureEmailNotBanned keeps an address banned in the request tenant from
// joining it again, even after the banned account deleted itself. The check
// uses the keyed marker (active and every previous key), never a plaintext
// address comparison or an unkeyed hash.
// provisionVerificationTx enqueues the verification email in the
// registration transaction. A service without a verification dependency is a
// minimal test wiring and sends nothing; one with it must have an outbox.
func (s *userService) provisionVerificationTx(ctx context.Context, tx TxRepos, user *User) error {
	if s.Verification == nil {
		return nil
	}
	provisioner, ok := s.Verification.(txProvisioner)
	if !ok || tx.Mail == nil {
		return apperror.NewInternal("failed to create user")
	}
	if err := provisioner.provisionTx(ctx, tx, user.ID, user.Email); err != nil {
		slog.Error("failed to provision registration verification email",
			slog.String("user_id", user.ID.String()),
			slog.Any("error", err),
		)
		// Provisioning details (including context deadlines) are for the logs
		// only. Keep the public registration contract at 500.
		return apperror.NewInternal("failed to create user")
	}
	return nil
}

func (s *userService) registeredEvent(ctx context.Context, userID uuid.UUID) audit.Event {
	event := userEvent(ctx, "auth.registered", userID, map[string]any{
		"email_verification_required": s.RequireEmailVerification,
	})
	event.ActorID = userID
	event.ActorType = audit.ActorTypeUser
	return event
}
