package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

// ResetRequestJobKind is the durable job that performs a forgot-password
// request: account lookup, token creation, and the email.send enqueue.
const ResetRequestJobKind = "password_reset.request"

// resetRequestMaxAttempts bounds retries of a transient lookup/enqueue failure.
const resetRequestMaxAttempts = 5

// resetTokenTTL is how long an emailed reset link stays valid.
const resetTokenTTL = time.Hour

type PasswordResetService interface {
	ForgotPassword(ctx context.Context, req dto.ForgotPasswordRequest) error
	ResetPassword(ctx context.Context, req dto.ResetPasswordRequest) error
	// HandleResetRequestJob processes one ResetRequestJobKind job. Delivery is
	// at-least-once: a duplicate run replaces the user's single reset token
	// (the newest emailed link wins), so it is safe to repeat.
	HandleResetRequestJob(ctx context.Context, payload json.RawMessage) error
}

// ResetRequestQueue is the durable queue ForgotPassword writes to.
// *jobs.Queue implements it.
type ResetRequestQueue interface {
	Enqueue(ctx context.Context, job jobs.Job) (int64, error)
}

// resetRequestPayload is the job payload. It carries the slug because the
// worker restores only the tenant ID, and the emailed link needs the slug.
type resetRequestPayload struct {
	Email      string `json:"email"`
	TenantSlug string `json:"tenantSlug,omitempty"`
}

type passwordResetService struct {
	userRepo    repository.UserRepository
	resetRepo   repository.PasswordResetRepository
	refreshRepo repository.RefreshTokenRepository
	txManager   *database.TxManager
	emailSender email.Sender
	cache       cache.Cache
	frontendURL string
	sessions    session.Store
	// requestQueue makes ForgotPassword constant-shape; requestCodec seals
	// the address in the job payload at rest (nil stores plaintext JSON).
	requestQueue ResetRequestQueue
	requestCodec *jobs.PayloadCodec
}

// AttachResetRequestQueue moves forgot-password work into a durable job.
// With a queue attached, ForgotPassword does the same work (rate-limit check
// plus one job insert) for every address and can only fail for reasons that
// do not depend on the account, so neither latency nor errors reveal whether
// an address is registered. Without one (unit tests, minimal wiring) requests
// are processed inline. Production wiring (identity.Build) always attaches it.
func AttachResetRequestQueue(svc PasswordResetService, queue ResetRequestQueue, codec *jobs.PayloadCodec) {
	if s, ok := svc.(*passwordResetService); ok {
		s.requestQueue = queue
		s.requestCodec = codec
	}
}

func NewPasswordResetService(
	userRepo repository.UserRepository,
	resetRepo repository.PasswordResetRepository,
	refreshRepo repository.RefreshTokenRepository,
	emailSender email.Sender,
	appCache cache.Cache,
	frontendURL string,
	txManager *database.TxManager,
	sessions ...session.Store,
) PasswordResetService {
	var sessionStore session.Store
	if len(sessions) > 0 {
		sessionStore = sessions[0]
	}
	return &passwordResetService{
		userRepo:    userRepo,
		resetRepo:   resetRepo,
		refreshRepo: refreshRepo,
		txManager:   txManager,
		emailSender: emailSender,
		cache:       appCache,
		frontendURL: frontendURL,
		sessions:    sessionStore,
	}
}

func (s *passwordResetService) ForgotPassword(ctx context.Context, req dto.ForgotPasswordRequest) error {
	emailAddr := NormalizeEmail(req.Email)

	// Rate limit before the lookup, for every address. Increment is atomic and
	// sets the TTL on the first request. Returning the same generic success
	// response for known/unknown/limited addresses avoids an enumeration oracle.
	cacheKey := scopedKey(ctx, "password_reset:", emailAddr)
	if s.cache != nil {
		if n, err := s.cache.Increment(ctx, cacheKey, 1*time.Minute); err == nil && n > 1 {
			return nil
		}
	}

	if s.requestQueue == nil {
		return s.processResetRequest(ctx, emailAddr)
	}
	return s.enqueueResetRequest(ctx, emailAddr)
}

// enqueueResetRequest writes the durable job without reading the account.
func (s *passwordResetService) enqueueResetRequest(ctx context.Context, emailAddr string) error {
	payload, err := json.Marshal(resetRequestPayload{Email: emailAddr, TenantSlug: tenant.SlugFromContext(ctx)})
	if err != nil {
		return apperror.NewInternal("failed to process request")
	}
	if s.requestCodec != nil {
		if payload, err = s.requestCodec.Seal(payload); err != nil {
			slog.Error("failed to seal password reset request", slog.Any("error", err))
			return apperror.NewInternal("failed to process request")
		}
	}
	if _, err := s.requestQueue.Enqueue(ctx, jobs.Job{
		Kind:        ResetRequestJobKind,
		Payload:     payload,
		MaxAttempts: resetRequestMaxAttempts,
	}); err != nil {
		slog.Error("failed to enqueue password reset request", slog.Any("error", err))
		return apperror.NewInternal("failed to process request")
	}
	return nil
}

func (s *passwordResetService) HandleResetRequestJob(ctx context.Context, payload json.RawMessage) error {
	raw := []byte(payload)
	if s.requestCodec != nil {
		opened, err := s.requestCodec.Open(payload)
		if err != nil {
			return fmt.Errorf("open password reset request: %w", err)
		}
		raw = opened
	}
	// A payload that does not decode or has no address can never succeed, so
	// it dead-letters immediately; decryption errors above stay retryable.
	var req resetRequestPayload
	if err := json.Unmarshal(raw, &req); err != nil {
		return jobs.Permanent(fmt.Errorf("decode password reset request: %w", err))
	}
	emailAddr := NormalizeEmail(req.Email)
	if emailAddr == "" {
		return jobs.Permanent(errors.New("password reset request has no email"))
	}
	// The worker restores the job's tenant ID; the slug only shapes the link.
	return s.processResetRequest(tenant.WithSlug(ctx, req.TenantSlug), emailAddr)
}

// processResetRequest looks the account up and, when it exists, replaces its
// reset token and enqueues the email in one transaction.
func (s *passwordResetService) processResetRequest(ctx context.Context, emailAddr string) error {
	// Unknown addresses succeed silently so callers cannot enumerate accounts.
	user, err := s.userRepo.GetByEmail(ctx, emailAddr)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil // Silent fail
		}
		return apperror.NewInternal("failed to process request")
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return apperror.NewInternal("failed to generate reset token")
	}
	token := hex.EncodeToString(b)

	resetURL := tokenLink(ctx, s.frontendURL, "reset-password", token)
	msg := email.Message{
		To:      []string{user.Email},
		Subject: "Password Reset Request",
		HTML:    fmt.Sprintf("<p>Click <a href=%q>here</a> to reset your password. This link expires in 1 hour.</p>", resetURL),
	}
	txSender, transactional := s.emailSender.(transactionalEmailSender)

	// issued is false when the account changed its address (or was deleted)
	// after the lookup: no token exists for the old address, so nothing is
	// sent. queuedInTx records that the email was enqueued on the token
	// transaction, so the fallback send below must not run as well.
	var issued, queuedInTx bool
	err = inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		created, createErr := replaceResetToken(ctx, r.resets, user, token)
		if createErr != nil || !created {
			return createErr
		}
		issued = true
		if tx == nil || !transactional {
			return nil
		}
		// Enqueue on the same transaction: a crash between the token insert
		// and the enqueue cannot leave a usable reset link without an email.
		if sendErr := txSender.SendTx(ctx, tx, msg); sendErr != nil {
			slog.Error("failed to enqueue password reset email",
				slog.Int64("user_id", user.ID),
				slog.Any("error", sendErr),
			)
			return apperror.NewInternal("failed to process request")
		}
		queuedInTx = true
		return nil
	})
	if err != nil {
		return err
	}
	if issued && !queuedInTx {
		s.sendResetEmailAsync(ctx, user, msg)
	}
	return nil
}

// replaceResetToken replaces the user's reset token with token. It reports
// false, without error, when the account no longer has the address the
// request was made for: CreatePasswordResetToken re-checks the address under
// a row lock, so an email change between the lookup and this insert can never
// mail a valid link to the previous address.
func replaceResetToken(ctx context.Context, resets repository.PasswordResetRepository, user *identitysqlc.User, token string) (bool, error) {
	// Delete old tokens and create the new one in a single transaction so two
	// concurrent requests cannot leave two live reset links behind.
	if err := resets.DeleteByUserID(ctx, user.ID); err != nil {
		return false, apperror.NewInternal("failed to clear old reset tokens")
	}
	_, err := resets.Create(ctx, identitysqlc.CreatePasswordResetTokenParams{
		TenantID:  tenant.EffectiveID(ctx),
		UserID:    user.ID,
		Token:     hashToken(token),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(resetTokenTTL), Valid: true},
		Email:     user.Email,
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, apperror.ErrNotFound):
		return false, nil
	default:
		return false, apperror.NewInternal("failed to create reset token")
	}
}

// sendResetEmailAsync sends the reset email after commit when the sender
// cannot enqueue transactionally. SMTP latency must not create a timing
// oracle and must not block the request; failures are logged, never
// returned. The request tenant is carried explicitly: a bare background
// context would enqueue the job under the default tenant.
func (s *passwordResetService) sendResetEmailAsync(ctx context.Context, user *identitysqlc.User, msg email.Message) {
	tenantID := user.TenantID
	if tenantID <= 0 {
		tenantID = tenant.EffectiveID(ctx)
	}
	tenantSlug := tenant.SlugFromContext(ctx)
	async.Go(func() {
		sendCtx := tenant.WithIdentity(context.Background(), tenantID, tenantSlug)
		if err := s.emailSender.Send(sendCtx, msg); err != nil {
			slog.Warn("failed to send password reset email",
				slog.Int64("user_id", user.ID),
				slog.Any("error", err),
			)
		}
	})
}

func (s *passwordResetService) fallbackRepos() repos {
	return repos{users: s.userRepo, refresh: s.refreshRepo, resets: s.resetRepo}
}

func (s *passwordResetService) ResetPassword(ctx context.Context, req dto.ResetPasswordRequest) error {
	tokenHash := hashToken(req.Token)

	// Validate the token with a cheap read before spending bcrypt CPU, and
	// hash outside the transaction so the cost-12 hash never holds a pooled
	// connection. The read is only a pre-check: single use is still enforced
	// by the atomic Consume inside the transaction below.
	if err := s.precheckResetToken(ctx, tokenHash); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return apperror.NewInternal("failed to hash password")
	}

	var userID int64
	err = inTx(ctx, s.txManager, s.fallbackRepos(), func(_ pgx.Tx, r repos) error {
		owner, consumeErr := consumeResetToken(ctx, r, tokenHash)
		if consumeErr != nil {
			return consumeErr
		}
		if resetErr := applyPasswordReset(ctx, r, owner, hash); resetErr != nil {
			return resetErr
		}
		userID = owner
		return nil
	})
	if err != nil {
		return err
	}

	// A successful reset must also clear any brute-force lockout so the user
	// can actually sign in with the new password.
	if s.cache != nil {
		if user, err := s.userRepo.GetByID(ctx, userID); err == nil {
			clearLoginAttempts(ctx, s.cache, NormalizeEmail(user.Email))
		}
	}
	invalidateSessions(ctx, s.sessions, userID)
	return nil
}

func invalidResetToken() error {
	return apperror.NewBadRequest("invalid or expired reset token")
}

// precheckResetToken rejects an unknown, cross-tenant, or expired token
// before any password hashing.
func (s *passwordResetService) precheckResetToken(ctx context.Context, tokenHash string) error {
	pending, err := s.resetRepo.GetByToken(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return invalidResetToken()
		}
		return apperror.NewInternal("failed to verify reset token")
	}
	if pending.TenantID != tenant.EffectiveID(ctx) {
		return invalidResetToken()
	}
	if pending.ExpiresAt.Time.Before(time.Now()) {
		// Delete outside any transaction so the cleanup persists now rather
		// than waiting for the scheduled expired-token purge.
		if err := s.resetRepo.Delete(ctx, tokenHash); err != nil {
			slog.Error("failed to delete expired reset token", slog.Any("error", err))
		}
		return apperror.NewBadRequest("reset token has expired")
	}
	return nil
}

// consumeResetToken atomically consumes the token and returns its user. This
// avoids the token->user lock-order deadlock that FOR UPDATE could create
// when two resets run concurrently, and a concurrent reset that consumed the
// token first makes this one fail.
func consumeResetToken(ctx context.Context, r repos, tokenHash string) (int64, error) {
	rt, err := r.resets.Consume(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return 0, invalidResetToken()
		}
		return 0, apperror.NewInternal("failed to verify reset token")
	}
	if rt.TenantID != tenant.EffectiveID(ctx) {
		return 0, invalidResetToken()
	}
	// Re-check: the token may have expired since the pre-check. The rollback
	// keeps it; the next attempt's pre-check deletes it.
	if rt.ExpiresAt.Time.Before(time.Now()) {
		return 0, apperror.NewBadRequest("reset token has expired")
	}

	currentUser, err := r.users.GetByID(ctx, rt.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return 0, invalidResetToken()
		}
		return 0, apperror.NewInternal("failed to load user")
	}
	// A password/email change after the link was issued invalidates it.
	if currentUser.PasswordChangedAt.Valid && rt.CreatedAt.Time.Before(currentUser.PasswordChangedAt.Time) {
		return 0, apperror.NewBadRequest("reset token has been invalidated by a later account change")
	}
	return rt.UserID, nil
}

// applyPasswordReset stores the new hash and revokes every reset and refresh
// token for the user. Deleting all reset tokens, not just the one used, keeps
// a sibling token from a concurrent request from resetting the password a
// second time.
func applyPasswordReset(ctx context.Context, r repos, userID int64, hash []byte) error {
	if _, err := r.users.UpdatePassword(ctx, identitysqlc.UpdateUserPasswordParams{
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
		ID:           userID,
	}); err != nil {
		return apperror.NewInternal("failed to update password")
	}
	if err := r.resets.DeleteByUserID(ctx, userID); err != nil {
		return apperror.NewInternal("failed to delete reset tokens")
	}
	if err := r.refresh.DeleteByUserID(ctx, userID); err != nil {
		return apperror.NewInternal("failed to revoke refresh tokens")
	}
	return nil
}
