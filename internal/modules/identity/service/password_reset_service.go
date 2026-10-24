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
	var req resetRequestPayload
	if err := json.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("decode password reset request: %w", err)
	}
	emailAddr := NormalizeEmail(req.Email)
	if emailAddr == "" {
		return errors.New("password reset request has no email")
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

	// Generate token
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

	// queuedInTx records that the email was enqueued on the token transaction,
	// so the fallback send below must not run as well.
	queuedInTx := false
	createToken := func(resetRepo repository.PasswordResetRepository, tx pgx.Tx) error {
		// Delete old tokens and create the new one in a single transaction so
		// two concurrent requests cannot leave two live reset links behind.
		if err := resetRepo.DeleteByUserID(ctx, user.ID); err != nil {
			return apperror.NewInternal("failed to clear old reset tokens")
		}
		_, err := resetRepo.Create(ctx, identitysqlc.CreatePasswordResetTokenParams{
			TenantID:  tenant.EffectiveID(ctx),
			UserID:    user.ID,
			Token:     hashToken(token),
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(1 * time.Hour), Valid: true},
		})
		if err != nil {
			return apperror.NewInternal("failed to create reset token")
		}
		if tx != nil && transactional {
			// Enqueue on the same transaction: a crash between the token insert
			// and the enqueue cannot leave a usable reset link without an email.
			if err := txSender.SendTx(ctx, tx, msg); err != nil {
				slog.Error("failed to enqueue password reset email",
					slog.Int64("user_id", user.ID),
					slog.Any("error", err),
				)
				return apperror.NewInternal("failed to process request")
			}
			queuedInTx = true
		}
		return nil
	}

	if s.txManager != nil {
		if err := s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return createToken(repository.NewPasswordResetRepository(tx), tx)
		}); err != nil {
			return err
		}
	} else if err := createToken(s.resetRepo, nil); err != nil {
		return err
	}

	if !queuedInTx {
		// Send email asynchronously: SMTP latency must not create a timing
		// oracle and must not block the request. Failures are logged, never
		// returned. Carry the request tenant explicitly: a bare background
		// context would enqueue the job under the default tenant.
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

	return nil
}

func (s *passwordResetService) ResetPassword(ctx context.Context, req dto.ResetPasswordRequest) error {
	tokenHash := hashToken(req.Token)
	var resetUserID int64

	// Validate the token with a cheap read before spending bcrypt CPU, and
	// hash outside the transaction so the cost-12 hash never holds a pooled
	// connection. The read is only a pre-check: single use is still enforced
	// by the atomic Consume inside the transaction below.
	pending, err := s.resetRepo.GetByToken(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewBadRequest("invalid or expired reset token")
		}
		return apperror.NewInternal("failed to verify reset token")
	}
	if pending.TenantID != tenant.EffectiveID(ctx) {
		return apperror.NewBadRequest("invalid or expired reset token")
	}
	if pending.ExpiresAt.Time.Before(time.Now()) {
		// Delete outside any transaction so the cleanup actually persists
		// (there is no TTL purge for reset tokens).
		if err := s.resetRepo.Delete(ctx, tokenHash); err != nil {
			slog.Error("failed to delete expired reset token", slog.Any("error", err))
		}
		return apperror.NewBadRequest("reset token has expired")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return apperror.NewInternal("failed to hash password")
	}

	doReset := func(userRepo repository.UserRepository, resetRepo repository.PasswordResetRepository, refreshRepo repository.RefreshTokenRepository) error {
		// Consume atomically. This avoids the token->user lock-order deadlock
		// that FOR UPDATE could create when two resets run concurrently, and a
		// concurrent reset that consumed the token first makes this one fail.
		rt, err := resetRepo.Consume(ctx, tokenHash)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewBadRequest("invalid or expired reset token")
			}
			return apperror.NewInternal("failed to verify reset token")
		}
		if rt.TenantID != tenant.EffectiveID(ctx) {
			return apperror.NewBadRequest("invalid or expired reset token")
		}
		// Re-check: the token may have expired since the pre-check. The
		// rollback keeps it; the next attempt's pre-check deletes it.
		if rt.ExpiresAt.Time.Before(time.Now()) {
			return apperror.NewBadRequest("reset token has expired")
		}

		currentUser, err := userRepo.GetByID(ctx, rt.UserID)
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewBadRequest("invalid or expired reset token")
			}
			return apperror.NewInternal("failed to load user")
		}
		// A password/email change after the link was issued invalidates it.
		if currentUser.PasswordChangedAt.Valid &&
			rt.CreatedAt.Time.Before(currentUser.PasswordChangedAt.Time) {
			return apperror.NewBadRequest("reset token has been invalidated by a later account change")
		}
		resetUserID = rt.UserID

		_, err = userRepo.UpdatePassword(ctx, identitysqlc.UpdateUserPasswordParams{
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			ID:           rt.UserID,
		})
		if err != nil {
			return apperror.NewInternal("failed to update password")
		}
		// Invalidate every reset token for this user, not just the one that
		// was used, so a sibling token created by a concurrent request cannot
		// reset the password a second time.
		if err := resetRepo.DeleteByUserID(ctx, rt.UserID); err != nil {
			return apperror.NewInternal("failed to delete reset tokens")
		}
		if err := refreshRepo.DeleteByUserID(ctx, rt.UserID); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	}

	if s.txManager != nil {
		err = s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doReset(
				repository.NewUserRepository(tx),
				repository.NewPasswordResetRepository(tx),
				repository.NewRefreshTokenRepository(tx),
			)
		})
	} else {
		err = doReset(s.userRepo, s.resetRepo, s.refreshRepo)
	}
	if err != nil {
		return err
	}

	if resetUserID != 0 {
		// A successful reset must also clear any brute-force lockout so the user
		// can actually sign in with the new password.
		if s.cache != nil {
			if user, err := s.userRepo.GetByID(ctx, resetUserID); err == nil {
				clearLoginAttempts(ctx, s.cache, NormalizeEmail(user.Email))
			}
		}
		if s.sessions != nil {
			_ = s.sessions.Invalidate(ctx, resetUserID)
		}
	}
	return nil
}
