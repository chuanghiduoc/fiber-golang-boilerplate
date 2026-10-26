package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

// ResetRequestJobKind is the durable job that performs a forgot-password
// request: account lookup, token creation, and the email.send enqueue.
const ResetRequestJobKind = "password_reset.request"

// resetRequestMaxAttempts bounds retries of a transient lookup/enqueue failure.
const resetRequestMaxAttempts = 5

// resetTokenTTL is how long an emailed reset link stays valid.
const resetTokenTTL = time.Hour

// PasswordResetService resets the (global) password. Reset tokens are per
// user: the API looks a token up by its hash alone, and the emailed link's
// tenant parameter only tells the frontend where to sign in afterwards.
type PasswordResetService interface {
	ForgotPassword(ctx context.Context, req ForgotPasswordInput) error
	ResetPassword(ctx context.Context, req ResetPasswordInput) error
	// HandleResetRequestJob processes one ResetRequestJobKind job. Delivery is
	// at-least-once: a duplicate run replaces the user's single reset token
	// (the newest emailed link wins), so it is safe to repeat.
	HandleResetRequestJob(ctx context.Context, payload json.RawMessage) error
}

// ForgotPasswordInput requests a reset link for an address.
type ForgotPasswordInput struct {
	Email string
}

// ResetPasswordInput consumes a reset token and sets a new password.
type ResetPasswordInput struct {
	Token    string
	Password string
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

// ResetDeps are the password reset service's collaborators.
type ResetDeps struct {
	Stores       Stores
	Tx           Transactor
	Sender       email.Sender
	Cache        cache.Cache
	FrontendURL  string
	SessionCache session.Store
	// Security sends the password-reset notification. Nil disables it.
	Security *SecurityNotifier
}

type passwordResetService struct {
	ResetDeps
	// requestQueue makes ForgotPassword constant-shape; requestCodec seals
	// the address in the job payload at rest (nil stores plaintext JSON).
	requestQueue ResetRequestQueue
	requestCodec *jobs.PayloadCodec
}

// NewPasswordResetService wires the reset service.
func NewPasswordResetService(deps ResetDeps) PasswordResetService {
	return &passwordResetService{ResetDeps: deps}
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

func (s *passwordResetService) ForgotPassword(ctx context.Context, req ForgotPasswordInput) error {
	emailAddr := NormalizeEmail(req.Email)

	// Rate limit before the lookup, for every address. Increment is atomic and
	// sets the TTL on the first request. Returning the same generic success
	// response for known/unknown/limited addresses avoids an enumeration oracle.
	if s.Cache != nil {
		if n, err := s.Cache.Increment(ctx, emailKey("password_reset:", emailAddr), time.Minute); err == nil && n > 1 {
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
	user, err := s.Stores.Users.GetByEmail(ctx, emailAddr)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil
		}
		return apperror.NewInternal("failed to process request")
	}
	if user.Status != UserStatusActive {
		return nil
	}

	token, tokenHash, err := newOpaqueToken()
	if err != nil {
		return apperror.NewInternal("failed to generate reset token")
	}
	resetURL := tokenLink(ctx, s.FrontendURL, "reset-password", token)
	msg := email.Message{
		To:      []string{user.Email},
		Subject: "Password Reset Request",
		HTML:    fmt.Sprintf("<p>Click <a href=%q>here</a> to reset your password. This link expires in 1 hour.</p>", resetURL),
	}

	// issued is false when the account changed its address (or was deleted)
	// after the lookup: no token exists for the old address, so nothing is
	// sent. queuedInTx records that the email was enqueued on the token
	// transaction, so the fallback send below must not run as well.
	var issued, queuedInTx bool
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		created, createErr := replaceResetToken(ctx, tx.Resets, user, tokenHash)
		if createErr != nil || !created {
			return createErr
		}
		issued = true
		if tx.Mail == nil {
			return nil
		}
		// Enqueue on the same transaction: a crash between the token insert
		// and the enqueue cannot leave a usable reset link without an email.
		if sendErr := tx.Mail.Send(ctx, msg); sendErr != nil {
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
		s.sendResetEmailAsync(ctx, user.ID, msg)
	}
	return nil
}

// replaceResetToken replaces the user's reset token. It reports false,
// without error, when the account no longer has the address the request was
// made for: the insert re-checks the address under a row lock, so an email
// change between the lookup and this insert can never mail a valid link to
// the previous address.
func replaceResetToken(ctx context.Context, resets PasswordResetRepository, user *User, tokenHash string) (bool, error) {
	created, err := resets.Create(ctx, user.ID, user.Email, tokenHash, time.Now().Add(resetTokenTTL))
	if err != nil {
		return false, apperror.NewInternal("failed to create reset token")
	}
	return created, nil
}

// sendResetEmailAsync sends the reset email after commit when the sender
// cannot enqueue transactionally. SMTP latency must not create a timing
// oracle and must not block the request; failures are logged, never
// returned. The request tenant is carried explicitly: a bare background
// context would enqueue the job under the default tenant.
func (s *passwordResetService) sendResetEmailAsync(ctx context.Context, userID int64, msg email.Message) {
	tenantID, tenantSlug := tenant.EffectiveID(ctx), tenant.SlugFromContext(ctx)
	async.Go(func() {
		sendCtx := tenant.WithIdentity(context.Background(), tenantID, tenantSlug)
		if err := s.Sender.Send(sendCtx, msg); err != nil {
			slog.Warn("failed to send password reset email",
				slog.Int64("user_id", userID),
				slog.Any("error", err),
			)
		}
	})
}

func (s *passwordResetService) ResetPassword(ctx context.Context, req ResetPasswordInput) error {
	tokenHash := hashToken(req.Token)

	// Validate the token with a cheap read before spending bcrypt CPU, and
	// hash outside the transaction so the cost-12 hash never holds a pooled
	// connection. The read is only a pre-check: single use is still enforced
	// by the atomic Consume inside the transaction below.
	if err := s.precheckResetToken(ctx, tokenHash); err != nil {
		return err
	}
	hash, err := hashPassword(req.Password)
	if err != nil {
		return err
	}

	var owner *User
	// The token consumption, the new password row, and the token-version bump
	// live in the request tenant's transaction. They are all global statements
	// (token hash / user id), so no RLS bypass is needed; the bump alone ends
	// every session in every tenant and the per-tenant session-row revocation
	// is recorded by the follow-up bypass statement below.
	err = s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		user, consumeErr := consumeResetToken(ctx, tx.Stores, tokenHash)
		if consumeErr != nil {
			return consumeErr
		}
		if resetErr := replacePassword(ctx, tx.Stores, user.ID, hash); resetErr != nil {
			return resetErr
		}
		if notifyErr := s.Security.Deliver(ctx, tx, SecurityNotice{
			Event:      SecurityEventPasswordReset,
			UserID:     user.ID,
			Name:       user.Name,
			Email:      user.Email,
			OccurredAt: time.Now().UTC(),
		}); notifyErr != nil {
			return notifyErr
		}
		owner = user
		return nil
	})
	if err != nil {
		return err
	}

	// A successful reset must also clear any brute-force lockout so the user
	// can actually sign in with the new password.
	cleanupStaleSessions(ctx, s.Stores.Sessions, owner.ID, RevokeCredentialsChanged)
	clearLoginAttempts(ctx, s.Cache, owner.Email)
	invalidateSessions(ctx, s.SessionCache, owner.ID)
	return nil
}

func invalidResetToken() error {
	return apperror.NewBadRequest("invalid or expired reset token")
}

// precheckResetToken rejects an unknown or expired token before any password
// hashing.
func (s *passwordResetService) precheckResetToken(ctx context.Context, tokenHash string) error {
	pending, err := s.Stores.Resets.GetByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return invalidResetToken()
		}
		return apperror.NewInternal("failed to verify reset token")
	}
	if !pending.ExpiresAt.After(time.Now()) {
		// Delete outside any transaction so the cleanup persists now rather
		// than waiting for the scheduled expired-token purge.
		if err := s.Stores.Resets.Delete(ctx, tokenHash); err != nil {
			slog.Error("failed to delete expired reset token", slog.Any("error", err))
		}
		return apperror.NewBadRequest("reset token has expired")
	}
	return nil
}

// consumeResetToken atomically consumes the token and returns its user. A
// concurrent reset that consumed the token first makes this one fail.
func consumeResetToken(ctx context.Context, st Stores, tokenHash string) (*User, error) {
	rt, err := st.Resets.Consume(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, invalidResetToken()
		}
		return nil, apperror.NewInternal("failed to verify reset token")
	}
	// Re-check: the token may have expired since the pre-check. The rollback
	// keeps it; the next attempt's pre-check deletes it.
	if !rt.ExpiresAt.After(time.Now()) {
		return nil, apperror.NewBadRequest("reset token has expired")
	}
	user, err := st.Users.GetByID(ctx, rt.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, invalidResetToken()
		}
		return nil, apperror.NewInternal("failed to load user")
	}
	if user.Status != UserStatusActive {
		return nil, invalidResetToken()
	}
	// A password/email change after the link was issued invalidates it.
	if user.CredentialsChangedAt != nil && rt.CreatedAt.Before(*user.CredentialsChangedAt) {
		return nil, apperror.NewBadRequest("reset token has been invalidated by a later account change")
	}
	return user, nil
}
