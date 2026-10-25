package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/events"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

func emailTaken() error {
	return apperror.NewCode(identitycatalog.CodeUserEmailTaken, "email already registered")
}

// Register creates a local account. The user, the optional verification
// email job, the auth.registered audit row, and the user.registered event
// commit in one transaction: any failure rolls the account back, so a
// registration never exists without its audit record or outbox rows.
func (s *userService) Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error) {
	email := NormalizeEmail(req.Email)
	name := strings.TrimSpace(req.Name)

	if err := s.ensureEmailUnregistered(ctx, email); err != nil {
		return nil, err
	}
	// Hash before opening a transaction: bcrypt is intentionally expensive and
	// must not hold a pooled database connection for hundreds of milliseconds.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return nil, apperror.NewInternal("failed to hash password")
	}

	var user *identitysqlc.User
	err = inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		created, createErr := s.createUser(ctx, r.users, email, name, hash)
		if createErr != nil {
			return createErr
		}
		if provErr := s.provisionVerificationTx(ctx, tx, created); provErr != nil {
			return provErr
		}
		if auditErr := checkAudit(audit.RecordInTx(ctx, s.audit, tx, s.registeredEvent(ctx, created))); auditErr != nil {
			return auditErr
		}
		if pubErr := s.publishRegisteredTx(ctx, tx, created); pubErr != nil {
			return pubErr
		}
		user = created
		return nil
	})
	if err != nil {
		return nil, err
	}

	// A previous failed-login lockout must not survive registration.
	clearLoginAttempts(ctx, s.cache, email)
	return ToUserResponse(user), nil
}

// ensureEmailUnregistered rejects an address that belongs to an active or a
// banned account in the request tenant.
func (s *userService) ensureEmailUnregistered(ctx context.Context, email string) error {
	_, err := s.repo.GetByEmail(ctx, email)
	switch {
	case err == nil:
		return emailTaken()
	case !errors.Is(err, apperror.ErrNotFound):
		return apperror.NewInternal("failed to check existing user")
	}
	return ensureEmailNotBanned(ctx, s.repo, email)
}

// ensureEmailNotBanned keeps a banned account's address from being reused
// while the ban stands (unban restores the original address).
func ensureEmailNotBanned(ctx context.Context, repo repository.UserRepository, email string) error {
	_, err := repo.GetBannedByEmail(ctx, email)
	switch {
	case err == nil:
		return apperror.NewConflict("email is not available")
	case errors.Is(err, apperror.ErrNotFound):
		return nil
	default:
		return apperror.NewInternal("failed to check email availability")
	}
}

// createUser creates a local account with a precomputed password hash through
// repo, which may be backed by the pool or by a caller-owned transaction.
func (s *userService) createUser(
	ctx context.Context,
	repo repository.UserRepository,
	email, name string,
	hash []byte,
) (*identitysqlc.User, error) {
	user, err := repo.Create(ctx, identitysqlc.CreateUserParams{
		TenantID:     tenant.EffectiveID(ctx),
		Email:        email,
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		Name:         name,
	})
	if err != nil {
		if database.IsUniqueViolation(err) {
			return nil, emailTaken()
		}
		return nil, apperror.NewInternal("failed to create user")
	}
	return user, nil
}

// provisionVerificationTx enqueues the verification email in the
// registration transaction when the provisioner supports it. Otherwise the
// handler sends it after commit (see RegistrationEmailsTransactional).
func (s *userService) provisionVerificationTx(ctx context.Context, tx pgx.Tx, user *identitysqlc.User) error {
	if tx == nil || s.verificationProvisioner == nil || !s.verificationProvisioner.SupportsTransactionalEnqueue() {
		return nil
	}
	if err := s.verificationProvisioner.SendVerificationTx(ctx, tx, user.ID, user.Email); err != nil {
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

func (s *userService) registeredEvent(ctx context.Context, user *identitysqlc.User) audit.Event {
	event := userEvent(ctx, "auth.registered", user.ID, map[string]any{
		"email_verification_required": s.requireEmailVerification,
	})
	event.ActorID = user.ID
	event.ActorType = audit.ActorTypeUser
	return event
}

// publishRegisteredTx publishes user.registered in the registration
// transaction, so the account never exists without its event (or vice versa)
// and every durable subscriber (webhooks) sees it. Without a bus or a
// transaction (unit tests) nothing is published.
func (s *userService) publishRegisteredTx(ctx context.Context, tx pgx.Tx, user *identitysqlc.User) error {
	if s.events == nil || tx == nil {
		return nil
	}
	event, err := userRegisteredEvent(user)
	if err == nil {
		err = s.events.PublishTx(ctx, tx, event)
	}
	if err != nil {
		slog.Error("failed to publish user.registered",
			slog.Int64("user_id", user.ID),
			slog.Any("error", err),
		)
		return apperror.NewInternal("failed to create user")
	}
	return nil
}

// userRegisteredEvent builds the user.registered envelope. The new user is the
// actor, matching the auth.registered audit row.
func userRegisteredEvent(user *identitysqlc.User) (events.Event, error) {
	event, err := events.NewEvent(identitycontracts.EventUserRegistered, identitycontracts.UserRegisteredVersion, identitycontracts.UserRegistered{
		UserID:        user.ID,
		Email:         user.Email,
		Name:          user.Name,
		EmailVerified: user.EmailVerifiedAt.Valid,
		CreatedAt:     user.CreatedAt.Time.UTC(),
	})
	if err != nil {
		return events.Event{}, err
	}
	event.TenantID = user.TenantID
	event.Actor = events.UserActor(user.ID)
	return event, nil
}
