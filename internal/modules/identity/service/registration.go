package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"golang.org/x/crypto/bcrypt"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

func emailTaken() error {
	return apperror.NewCode(identitycatalog.CodeUserEmailTaken, "email already registered")
}

func emailUnavailable() error {
	return apperror.NewConflict("email is not available")
}

// Register creates a global user with a password and an active membership
// in the request tenant. An address that already has an account gets 409
// user.email_taken even when that account is not a member of this tenant:
// an existing user joins another tenant by accepting an invitation, never by
// registering a second account. The user, membership, optional verification
// email job, auth.registered audit row, and user.registered event commit in
// one transaction: any failure rolls the account back, so a registration
// never exists without its audit record or outbox rows.
func (s *userService) Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error) {
	email := NormalizeEmail(req.Email)
	name := strings.TrimSpace(req.Name)

	if err := ensureEmailUnregistered(ctx, s.Stores, email); err != nil {
		return nil, err
	}
	// Hash before opening a transaction: bcrypt is intentionally expensive and
	// must not hold a pooled database connection for hundreds of milliseconds.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return nil, apperror.NewInternal("failed to hash password")
	}

	var member *Member
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		created, createErr := createAccount(ctx, tx.Stores, NewUser{Email: email, Name: name}, string(hash), RoleMember)
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
	return ToUserResponse(member), nil
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
	return ensureEmailNotBanned(ctx, st.Memberships, email)
}

// ensureEmailNotBanned keeps an address banned in the request tenant from
// joining it again, even after the banned account deleted itself.
func ensureEmailNotBanned(ctx context.Context, memberships MembershipRepository, email string) error {
	banned, err := memberships.EmailBanned(ctx, email)
	switch {
	case err != nil:
		return apperror.NewInternal("failed to check email availability")
	case banned:
		return emailUnavailable()
	default:
		return nil
	}
}

// createAccount creates a global user, its password (when hash is set), and
// a membership with role in the request tenant through st.
func createAccount(ctx context.Context, st Stores, user NewUser, hash, role string) (*Member, error) {
	created, err := st.Users.Create(ctx, user)
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			return nil, emailTaken()
		}
		return nil, apperror.NewInternal("failed to create user")
	}
	if hash != "" {
		if err := st.Users.CreatePassword(ctx, created.ID, hash); err != nil {
			return nil, apperror.NewInternal("failed to create user")
		}
		created.HasPassword = true
	}
	membership, err := st.Memberships.Create(ctx, created.ID, role)
	if err != nil {
		return nil, apperror.NewInternal("failed to create membership")
	}
	return &Member{User: *created, Membership: *membership}, nil
}

// provisionVerificationTx enqueues the verification email in the
// registration transaction when the email outbox can join it. Otherwise the
// handler sends it after commit (see RegistrationEmailsTransactional).
func (s *userService) provisionVerificationTx(ctx context.Context, tx TxRepos, user *User) error {
	provisioner, ok := s.Verification.(txProvisioner)
	if !ok || tx.Mail == nil {
		return nil
	}
	if err := provisioner.provisionTx(ctx, tx, user.ID, user.Email); err != nil {
		slog.Error("failed to provision registration verification email",
			slog.Int64("user_id", user.ID),
			slog.Any("error", err),
		)
		// Provisioning details (including context deadlines) are for the logs
		// only. Keep the public registration contract at 500.
		return apperror.NewInternal("failed to create user")
	}
	return nil
}

func (s *userService) registeredEvent(ctx context.Context, userID int64) audit.Event {
	event := userEvent(ctx, "auth.registered", userID, map[string]any{
		"email_verification_required": s.RequireEmailVerification,
	})
	event.ActorID = userID
	event.ActorType = audit.ActorTypeUser
	return event
}
