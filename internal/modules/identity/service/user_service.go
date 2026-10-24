package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

const (
	bcryptCost         = 12
	maxLoginAttempts   = 5
	lockoutDuration    = 15 * time.Minute
	loginAttemptPrefix = "login_attempts:"
)

// dummyPasswordHash equalizes the bcrypt work for unknown-email logins so the
// response time does not reveal whether an account exists.
var dummyPasswordHash = func() []byte {
	hash, _ := bcrypt.GenerateFromPassword([]byte("timing-only-dummy-password"), bcryptCost)
	return hash
}()

type UserService interface {
	Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error)
	Authenticate(ctx context.Context, req dto.LoginRequest) (*dto.UserResponse, error)
	FindOrCreateByGoogle(ctx context.Context, googleID, email, name string) (*dto.UserResponse, error)
	GetByID(ctx context.Context, id int64) (*dto.UserResponse, error)
	List(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error)
	Update(ctx context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error)
	VerifyPassword(ctx context.Context, userID int64, password string) error
	Delete(ctx context.Context, id int64) error
	ChangePassword(ctx context.Context, userID int64, req dto.ChangePasswordRequest) error
	RevokeSessions(ctx context.Context, userID int64) error
	// SetVerificationProvisioner wires registration email provisioning into the
	// user-creation transaction. When unset, or when the provisioner cannot
	// enqueue transactionally, registration falls back to the handler's
	// post-commit verification email.
	SetVerificationProvisioner(p VerificationProvisioner)
	// RegistrationEmailsTransactional reports whether Register commits the
	// verification email with the user. Callers must not send a second email
	// when it returns true.
	RegistrationEmailsTransactional() bool
}

type userService struct {
	repo                     repository.UserRepository
	refreshTokenRepo         repository.RefreshTokenRepository
	requireEmailVerification bool
	cache                    cache.Cache
	txManager                *database.TxManager
	audit                    audit.TxRecorder
	sessions                 session.Store
	verificationProvisioner  VerificationProvisioner
	webhooks                 webhookcontracts.Dispatcher
}

func NewUserService(
	repo repository.UserRepository,
	refreshTokenRepo repository.RefreshTokenRepository,
	requireEmailVerification bool,
	appCache cache.Cache,
	txManager *database.TxManager,
	auditRecorder audit.TxRecorder,
	sessions ...session.Store,
) UserService {
	var sessionStore session.Store
	if len(sessions) > 0 {
		sessionStore = sessions[0]
	}
	return &userService{
		repo:                     repo,
		refreshTokenRepo:         refreshTokenRepo,
		requireEmailVerification: requireEmailVerification,
		cache:                    appCache,
		txManager:                txManager,
		audit:                    auditRecorder,
		sessions:                 sessionStore,
	}
}

func (s *userService) Register(ctx context.Context, req dto.RegisterRequest) (*dto.UserResponse, error) {
	email := normalizeEmail(req.Email)
	name := strings.TrimSpace(req.Name)

	existing, err := s.repo.GetByEmail(ctx, email)
	if err != nil && !errors.Is(err, apperror.ErrNotFound) {
		return nil, apperror.NewInternal("failed to check existing user")
	}
	if existing != nil {
		return nil, apperror.NewCode(identitycatalog.CodeUserEmailTaken, "email already registered")
	}
	if err := ensureEmailNotBanned(ctx, s.repo, email); err != nil {
		return nil, err
	}

	// Hash before opening a transaction: bcrypt is intentionally expensive and
	// must not hold a pooled database connection for hundreds of milliseconds.
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return nil, apperror.NewInternal("failed to hash password")
	}

	var (
		user          *identitysqlc.User
		webhookQueued bool
	)
	if s.txManager != nil {
		// Create the user, optional verification provisioning, and the audit
		// row in one transaction. Any failure rolls the account back: a
		// successful registration must never exist without its audit record.
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			created, createErr := s.createUser(ctx, repository.NewUserRepository(tx), email, name, hash)
			if createErr != nil {
				return createErr
			}
			if s.verificationProvisioner != nil && s.verificationProvisioner.SupportsTransactionalEnqueue() {
				if provErr := s.verificationProvisioner.SendVerificationTx(ctx, tx, created.ID, created.Email); provErr != nil {
					slog.Error("failed to provision registration verification email",
						slog.Int64("user_id", created.ID),
						slog.Any("error", provErr),
					)
					// Provisioning details (including context deadlines) are for
					// the logs only. Keep the public registration contract at 500.
					return apperror.NewInternal("failed to create user")
				}
			}
			event := audit.NewServiceEvent(ctx, "auth.registered", "user")
			event.ResourceID = strconv.FormatInt(created.ID, 10)
			event.ActorID = created.ID
			event.ActorType = audit.ActorTypeUser
			event.Metadata = map[string]any{
				"email_verification_required": s.requireEmailVerification,
			}
			if auditErr := audit.RecordInTx(ctx, s.audit, tx, event); auditErr != nil {
				return auditWriteError(auditErr)
			}
			if txDispatcher, ok := s.webhooks.(WebhookTxDispatcher); ok {
				// The webhook outbox job commits with the account, so a
				// registration never exists without its event (or vice versa).
				if hookErr := dispatchWebhookTx(ctx, txDispatcher, tx, webhookcontracts.EventUserRegistered, created.TenantID, ToUserResponse(created)); hookErr != nil {
					slog.Error("failed to enqueue registration webhook",
						slog.Int64("user_id", created.ID),
						slog.Any("error", hookErr),
					)
					return apperror.NewInternal("failed to create user")
				}
				webhookQueued = true
			}
			user = created
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		user, err = s.createUser(ctx, s.repo, email, name, hash)
		if err != nil {
			return nil, err
		}
		event := audit.NewServiceEvent(ctx, "auth.registered", "user")
		event.ResourceID = strconv.FormatInt(user.ID, 10)
		event.ActorID = user.ID
		event.ActorType = audit.ActorTypeUser
		event.Metadata = map[string]any{
			"email_verification_required": s.requireEmailVerification,
		}
		if auditErr := audit.RecordInTx(ctx, s.audit, nil, event); auditErr != nil {
			return nil, auditWriteError(auditErr)
		}
	}

	// A previous failed-login lockout must not survive registration.
	_ = s.cache.Delete(ctx, scopedKey(ctx, loginAttemptPrefix, email))

	response := ToUserResponse(user)
	if !webhookQueued {
		s.dispatchWebhook(ctx, webhookcontracts.EventUserRegistered, user.TenantID, response)
	}
	return response, nil
}

// WebhookTxDispatcher is the optional transactional side of the webhooks
// dispatcher. It cannot live in the stdlib-only contracts package because it
// takes the producer's pgx.Tx.
type WebhookTxDispatcher interface {
	DispatchTx(ctx context.Context, tx pgx.Tx, events ...webhookcontracts.Event) error
}

// dispatchWebhookTx enqueues one domain event inside tx.
func dispatchWebhookTx(ctx context.Context, dispatcher WebhookTxDispatcher, tx pgx.Tx, eventType string, tenantID int64, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}
	return dispatcher.DispatchTx(ctx, tx, webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	})
}

// SetWebhookDispatcher wires outbound domain events.
func (s *userService) SetWebhookDispatcher(dispatcher webhookcontracts.Dispatcher) {
	s.webhooks = dispatcher
}

// AttachWebhookDispatcher wires the production dispatcher without widening the
// UserService interface, so handler fakes remain small.
func AttachWebhookDispatcher(svc UserService, dispatcher webhookcontracts.Dispatcher) {
	if s, ok := svc.(*userService); ok {
		s.webhooks = dispatcher
	}
}

// dispatchWebhook emits one best-effort domain event. Delivery failures are
// logged; they must not turn a committed registration into an API error.
func (s *userService) dispatchWebhook(ctx context.Context, eventType string, tenantID int64, data any) {
	if s.webhooks == nil {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		slog.Warn("failed to marshal webhook payload", slog.String("event", eventType), slog.Any("error", err))
		return
	}
	if _, err := s.webhooks.Dispatch(context.WithoutCancel(ctx), webhookcontracts.Event{
		Type:     eventType,
		TenantID: tenantID,
		Data:     payload,
	}); err != nil {
		slog.Warn("failed to dispatch webhook event", slog.String("event", eventType), slog.Any("error", err))
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
			return nil, apperror.NewCode(identitycatalog.CodeUserEmailTaken, "email already registered")
		}
		return nil, apperror.NewInternal("failed to create user")
	}
	return user, nil
}

func (s *userService) SetVerificationProvisioner(p VerificationProvisioner) {
	s.verificationProvisioner = p
}

func (s *userService) RegistrationEmailsTransactional() bool {
	return s.verificationProvisioner != nil &&
		s.txManager != nil &&
		s.verificationProvisioner.SupportsTransactionalEnqueue()
}

func (s *userService) Authenticate(ctx context.Context, req dto.LoginRequest) (*dto.UserResponse, error) {
	email := normalizeEmail(req.Email)

	// Reserve an attempt before any lookup or password check. The counter is
	// keyed by the normalized address whether or not an account exists, so a
	// locked response cannot reveal existence, and the atomic increment means
	// parallel guesses cannot all pass a stale "not locked yet" read.
	// Registration clears the key, so pre-locking an unregistered address
	// cannot lock out its future owner.
	cacheKey := scopedKey(ctx, loginAttemptPrefix, email)
	if err := s.reserveLoginAttempt(ctx, cacheKey); err != nil {
		return nil, err
	}

	user, err := s.repo.GetByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			// Spend the same bcrypt time as an existing account so login
			// timing cannot be used for account enumeration.
			_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(req.Password))
			return nil, apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	if !user.PasswordHash.Valid {
		// OAuth-only account: equalize timing with a password check.
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(req.Password))
		return nil, apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(req.Password)); err != nil {
		return nil, apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
	}

	// The password was correct: clear the counter even if the account still
	// needs verification, so a correct password never accrues a lockout.
	s.clearLoginAttempts(ctx, cacheKey)

	if s.requireEmailVerification && !user.EmailVerifiedAt.Valid {
		return nil, apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}
	return ToUserResponse(user), nil
}

// reserveLoginAttempt atomically counts one password attempt and rejects it
// once more than maxLoginAttempts were made inside the lockout window. It
// fails closed: when the cache is unavailable the attempt is denied rather
// than allowed without brute-force protection.
func (s *userService) reserveLoginAttempt(ctx context.Context, cacheKey string) error {
	attempts, err := s.cache.Increment(ctx, cacheKey, lockoutDuration)
	if err != nil {
		slog.Error("cache unavailable for login lockout check — denying request for safety", slog.Any("error", err))
		return apperror.NewInternal("unable to verify login attempts, please try again later")
	}
	if attempts > maxLoginAttempts {
		return apperror.NewTooManyRequests(fmt.Sprintf("account temporarily locked, try again in %d minutes", int(lockoutDuration.Minutes())))
	}
	return nil
}

func (s *userService) clearLoginAttempts(ctx context.Context, cacheKey string) {
	if err := s.cache.Delete(ctx, cacheKey); err != nil {
		slog.Warn("cache error clearing login attempts", slog.Any("error", err))
	}
}

func (s *userService) FindOrCreateByGoogle(ctx context.Context, googleID, email, name string) (*dto.UserResponse, error) {
	user, err := s.findOrCreateByGoogleUser(ctx, googleID, email, name)
	if err != nil {
		return nil, err
	}
	return ToUserResponse(user), nil
}

func (s *userService) findOrCreateByGoogleUser(ctx context.Context, googleID, email, name string) (*identitysqlc.User, error) {
	email = normalizeEmail(email)

	findOrCreate := func(repo repository.UserRepository) (*identitysqlc.User, error) {
		user, err := repo.GetByGoogleID(ctx, googleID)
		if err == nil {
			return user, nil
		}
		if !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to find user by google id")
		}

		existing, err := repo.GetByEmail(ctx, email)
		if err == nil {
			// Never silently attach a Google identity to an unverified local
			// account. An attacker could otherwise pre-register a victim's
			// address and take over the account when the victim uses Google.
			if !existing.EmailVerifiedAt.Valid {
				return nil, apperror.NewConflict("an account with this email already exists; verify it before linking Google")
			}
			linked, linkErr := repo.LinkGoogleAccount(ctx, identitysqlc.LinkGoogleAccountParams{
				GoogleID: pgtype.Text{String: googleID, Valid: true},
				ID:       existing.ID,
			})
			if linkErr != nil {
				if database.IsUniqueViolation(linkErr) {
					return nil, apperror.NewConflict("this Google account is already linked to another user")
				}
				return nil, apperror.NewInternal("failed to link google account")
			}
			return linked, nil
		}
		if !errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewInternal("failed to find user by email")
		}
		if err := ensureEmailNotBanned(ctx, repo, email); err != nil {
			return nil, err
		}

		newUser, err := repo.CreateOAuthUser(ctx, identitysqlc.CreateOAuthUserParams{
			TenantID:     tenant.EffectiveID(ctx),
			Email:        email,
			Name:         name,
			GoogleID:     pgtype.Text{String: googleID, Valid: true},
			AuthProvider: "google",
		})
		if err != nil {
			return nil, err
		}
		return newUser, nil
	}

	if s.txManager != nil {
		var result *identitysqlc.User
		txErr := s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			txUserRepo := repository.NewUserRepository(tx)
			var err error
			result, err = findOrCreate(txUserRepo)
			return err
		})
		if txErr != nil {
			if user, retryErr := s.retryOAuthLookup(ctx, googleID, email, txErr); retryErr == nil {
				if user != nil {
					return user, nil
				}
			} else {
				var appErr *apperror.AppError
				if errors.As(retryErr, &appErr) {
					return nil, retryErr
				}
			}
			return nil, apperror.NewInternal("failed to create oauth user")
		}
		return result, nil
	}

	result, err := findOrCreate(s.repo)
	if err != nil {
		if user, retryErr := s.retryOAuthLookup(ctx, googleID, email, err); retryErr == nil {
			if user != nil {
				return user, nil
			}
		} else {
			var appErr *apperror.AppError
			if errors.As(retryErr, &appErr) {
				return nil, retryErr
			}
		}
		return nil, apperror.NewInternal("failed to create oauth user")
	}
	return result, nil
}

// retryOAuthLookup handles the concurrent-create race. It returns a user when
// another request won, or nil to let the caller surface the original error.
func (s *userService) retryOAuthLookup(ctx context.Context, googleID, email string, cause error) (*identitysqlc.User, error) {
	if !database.IsUniqueViolation(cause) {
		return nil, cause
	}
	if user, err := s.repo.GetByGoogleID(ctx, googleID); err == nil {
		return user, nil
	}
	if _, err := s.repo.GetByEmail(ctx, email); err == nil {
		return nil, apperror.NewConflict("an account with this email already exists; verify it before linking Google")
	}
	return nil, cause
}

func (s *userService) GetByID(ctx context.Context, id int64) (*dto.UserResponse, error) {
	user, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	return ToUserResponse(user), nil
}

func (s *userService) List(ctx context.Context, limit int, startingAfter string) ([]dto.UserResponse, bool, error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}

	users, err := s.repo.ListCursor(ctx, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list users")
	}

	hasMore := len(users) > pageSize
	if hasMore {
		users = users[:pageSize]
	}

	responses := make([]dto.UserResponse, len(users))
	for i := range users {
		responses[i] = *ToUserResponse(&users[i])
	}

	return responses, hasMore, nil
}

func (s *userService) Update(ctx context.Context, id int64, req dto.UpdateUserRequest) (*dto.UserResponse, error) {
	existing, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, apperror.NewNotFound("user not found")
		}
		return nil, apperror.NewInternal("failed to get user")
	}

	name := existing.Name
	email := existing.Email

	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Email != nil {
		requested := normalizeEmail(*req.Email)
		if requested != normalizeEmail(existing.Email) {
			dup, err := s.repo.GetByEmail(ctx, requested)
			if err != nil && !errors.Is(err, apperror.ErrNotFound) {
				return nil, apperror.NewInternal("failed to check email availability")
			}
			if dup != nil {
				return nil, apperror.NewConflict("email already in use")
			}
			if err := ensureEmailNotBanned(ctx, s.repo, requested); err != nil {
				return nil, err
			}
			email = requested
		}
	}

	emailChanged := normalizeEmail(existing.Email) != normalizeEmail(email)
	nameChanged := req.Name != nil && strings.TrimSpace(*req.Name) != existing.Name

	doUpdate := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) (*identitysqlc.User, error) {
		user, err := userRepo.Update(ctx, identitysqlc.UpdateUserParams{
			ID:          id,
			Name:        name,
			Email:       email,
			UpdateEmail: req.Email != nil,
		})
		if err != nil {
			if database.IsUniqueViolation(err) {
				return nil, apperror.NewConflict("email already in use")
			}
			return nil, apperror.NewInternal("failed to update user")
		}
		if emailChanged {
			// Changing the identity address must end every existing session.
			if err := refreshRepo.DeleteByUserID(ctx, id); err != nil {
				return nil, apperror.NewInternal("failed to revoke sessions")
			}
		}
		return user, nil
	}

	auditEvent := func() audit.Event {
		event := audit.NewServiceEvent(ctx, "user.profile_updated", "user")
		event.ResourceID = strconv.FormatInt(id, 10)
		event.Metadata = map[string]any{
			"email_changed": emailChanged,
			"name_changed":  nameChanged,
		}
		return event
	}

	var user *identitysqlc.User
	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			var updateErr error
			user, updateErr = doUpdate(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
			if updateErr != nil {
				return updateErr
			}
			if auditErr := audit.RecordInTx(ctx, s.audit, tx, auditEvent()); auditErr != nil {
				return auditWriteError(auditErr)
			}
			return nil
		})
	} else {
		user, err = doUpdate(s.repo, s.refreshTokenRepo)
		if err == nil {
			if auditErr := audit.RecordInTx(ctx, s.audit, nil, auditEvent()); auditErr != nil {
				return nil, auditWriteError(auditErr)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if emailChanged && s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}

	return ToUserResponse(user), nil
}

// VerifyPassword checks a plaintext password against the stored hash. It is
// used to require re-authentication for sensitive profile changes.
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

func (s *userService) VerifyPassword(ctx context.Context, userID int64, password string) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to get user")
	}
	if !user.PasswordHash.Valid {
		return apperror.NewBadRequest("current password is incorrect")
	}

	// Shares the login counter so re-authentication cannot be used to guess
	// the password in parallel either.
	cacheKey := scopedKey(ctx, loginAttemptPrefix, normalizeEmail(user.Email))
	if err := s.reserveLoginAttempt(ctx, cacheKey); err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(password)) != nil {
		return apperror.NewBadRequest("current password is incorrect")
	}
	s.clearLoginAttempts(ctx, cacheKey)
	return nil
}

func (s *userService) Delete(ctx context.Context, id int64) error {
	target, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to get user")
	}
	doDelete := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		// Checked on the transaction's repository, under the per-tenant
		// admin-change lock, so it serializes with concurrent demotions/bans.
		if target.Role == dto.RoleAdmin {
			if err := ensureNotLastAdmin(ctx, userRepo, "cannot delete the last active admin"); err != nil {
				return err
			}
		}
		_, err := userRepo.Delete(ctx, id)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			var appErr *apperror.AppError
			if errors.As(err, &appErr) {
				return err
			}
			return apperror.NewInternal("failed to delete user")
		}
		// Revoke all refresh tokens so a deleted user's sessions cannot outlive
		// the account. Failing here rolls back the delete when run in a tx.
		if err := refreshRepo.DeleteByUserID(ctx, id); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	}

	auditEvent := func() audit.Event {
		event := audit.NewServiceEvent(ctx, "user.deleted", "user")
		event.ResourceID = strconv.FormatInt(id, 10)
		return event
	}

	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			if deleteErr := doDelete(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx)); deleteErr != nil {
				return deleteErr
			}
			if auditErr := audit.RecordInTx(ctx, s.audit, tx, auditEvent()); auditErr != nil {
				return auditWriteError(auditErr)
			}
			return nil
		})
	} else {
		err = doDelete(s.repo, s.refreshTokenRepo)
		if err == nil {
			if auditErr := audit.RecordInTx(ctx, s.audit, nil, auditEvent()); auditErr != nil {
				return auditWriteError(auditErr)
			}
		}
	}
	if err != nil {
		return err
	}

	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, id)
	}
	return nil
}

// RevokeSessions invalidates every access and refresh token for a user, used
// by the "logout all devices" endpoint.
func (s *userService) RevokeSessions(ctx context.Context, userID int64) error {
	doRevoke := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		if _, err := userRepo.RevokeSessions(ctx, userID); err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewNotFound("user not found")
			}
			return apperror.NewInternal("failed to revoke sessions")
		}
		if err := refreshRepo.DeleteByUserID(ctx, userID); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	}

	var err error
	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doRevoke(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx))
		})
	} else {
		err = doRevoke(s.repo, s.refreshTokenRepo)
	}
	if err != nil {
		return err
	}
	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, userID)
	}
	return nil
}

func (s *userService) ChangePassword(ctx context.Context, userID int64, req dto.ChangePasswordRequest) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewNotFound("user not found")
		}
		return apperror.NewInternal("failed to get user")
	}

	if !user.PasswordHash.Valid {
		return apperror.NewBadRequest("cannot change password for OAuth accounts")
	}

	if err := s.VerifyPassword(ctx, userID, req.CurrentPassword); err != nil {
		return err
	}
	if req.CurrentPassword == req.NewPassword {
		return apperror.NewBadRequest("new password must be different from the current password")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		return apperror.NewInternal("failed to hash password")
	}

	doChange := func(userRepo repository.UserRepository, refreshRepo repository.RefreshTokenRepository) error {
		if _, err := userRepo.UpdatePassword(ctx, identitysqlc.UpdateUserPasswordParams{
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			ID:           userID,
		}); err != nil {
			return apperror.NewInternal("failed to update password")
		}
		// A password change must invalidate every existing session.
		if err := refreshRepo.DeleteByUserID(ctx, userID); err != nil {
			return apperror.NewInternal("failed to revoke sessions")
		}
		return nil
	}

	auditEvent := func() audit.Event {
		event := audit.NewServiceEvent(ctx, "user.password_changed", "user")
		event.ResourceID = strconv.FormatInt(userID, 10)
		return event
	}

	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			if changeErr := doChange(repository.NewUserRepository(tx), repository.NewRefreshTokenRepository(tx)); changeErr != nil {
				return changeErr
			}
			if auditErr := audit.RecordInTx(ctx, s.audit, tx, auditEvent()); auditErr != nil {
				return auditWriteError(auditErr)
			}
			return nil
		})
	} else {
		err = doChange(s.repo, s.refreshTokenRepo)
		if err == nil {
			if auditErr := audit.RecordInTx(ctx, s.audit, nil, auditEvent()); auditErr != nil {
				return auditWriteError(auditErr)
			}
		}
	}
	if err != nil {
		return err
	}

	if s.sessions != nil {
		_ = s.sessions.Invalidate(ctx, userID)
	}
	_ = s.cache.Delete(ctx, scopedKey(ctx, loginAttemptPrefix, normalizeEmail(user.Email)))

	return nil
}

func ToUserResponse(user *identitysqlc.User) *dto.UserResponse {
	resp := &dto.UserResponse{
		ID:            user.ID,
		Email:         user.Email,
		Name:          user.Name,
		Role:          user.Role,
		EmailVerified: user.EmailVerifiedAt.Valid,
		HasPassword:   user.PasswordHash.Valid,
		CreatedAt:     user.CreatedAt.Time,
		UpdatedAt:     user.UpdatedAt.Time,
		TokenVersion:  user.TokenVersion,
		TenantID:      user.TenantID,
	}
	if user.DeletedAt.Valid {
		deletedAt := user.DeletedAt.Time
		resp.DeletedAt = &deletedAt
	}
	if user.BannedAt.Valid {
		bannedAt := user.BannedAt.Time
		resp.BannedAt = &bannedAt
	}
	return resp
}
