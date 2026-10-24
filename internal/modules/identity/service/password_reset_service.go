package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

type PasswordResetService interface {
	ForgotPassword(ctx context.Context, req dto.ForgotPasswordRequest) error
	ResetPassword(ctx context.Context, req dto.ResetPasswordRequest) error
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
	emailAddr := normalizeEmail(req.Email)

	// Rate limit before the lookup, for every address. Increment is atomic and
	// sets the TTL on the first request. Returning the same generic success
	// response for known/unknown/limited addresses avoids an enumeration oracle.
	cacheKey := scopedKey(ctx, "password_reset:", emailAddr)
	if s.cache != nil {
		if n, err := s.cache.Increment(ctx, cacheKey, 1*time.Minute); err == nil && n > 1 {
			return nil
		}
	}

	// Always return success to prevent email enumeration
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

	resetURL := fmt.Sprintf("%s/reset-password?token=%s", s.frontendURL, token)
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
					slog.String("email", user.Email),
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
		// returned.
		async.Go(func() {
			if err := s.emailSender.Send(context.Background(), msg); err != nil {
				slog.Warn("failed to send password reset email",
					slog.Int64("user_id", user.ID),
					slog.String("email", user.Email),
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

	doReset := func(userRepo repository.UserRepository, resetRepo repository.PasswordResetRepository, refreshRepo repository.RefreshTokenRepository) error {
		// Consume atomically. This avoids the token->user lock-order deadlock
		// that FOR UPDATE could create when two resets run concurrently.
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

		if rt.ExpiresAt.Time.Before(time.Now()) {
			if err := resetRepo.Delete(ctx, tokenHash); err != nil {
				slog.Error("failed to delete expired reset token", slog.Any("error", err))
			}
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

		// Hash only after the token has been validated, so invalid-token
		// requests cannot consume bcrypt CPU.
		hash, hashErr := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
		if hashErr != nil {
			return apperror.NewInternal("failed to hash password")
		}

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

	var err error
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
				_ = s.cache.Delete(ctx, scopedKey(ctx, loginAttemptPrefix, user.Email))
			}
		}
		if s.sessions != nil {
			_ = s.sessions.Invalidate(ctx, resetUserID)
		}
	}
	return nil
}
