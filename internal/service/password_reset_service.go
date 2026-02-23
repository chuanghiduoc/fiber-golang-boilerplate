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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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
}

func NewPasswordResetService(
	userRepo repository.UserRepository,
	resetRepo repository.PasswordResetRepository,
	refreshRepo repository.RefreshTokenRepository,
	emailSender email.Sender,
	appCache cache.Cache,
	frontendURL string,
	txManager *database.TxManager,
) PasswordResetService {
	return &passwordResetService{
		userRepo:    userRepo,
		resetRepo:   resetRepo,
		refreshRepo: refreshRepo,
		txManager:   txManager,
		emailSender: emailSender,
		cache:       appCache,
		frontendURL: frontendURL,
	}
}

func (s *passwordResetService) ForgotPassword(ctx context.Context, req dto.ForgotPasswordRequest) error {
	// Rate limit: 1 request per email per minute
	cacheKey := "password_reset:" + req.Email
	exists, _ := s.cache.Exists(ctx, cacheKey)
	if exists {
		return apperror.NewBadRequest("please wait before requesting another password reset")
	}

	// Always return success to prevent email enumeration
	user, err := s.userRepo.GetByEmail(ctx, req.Email)
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

	// Delete old tokens for this user
	_ = s.resetRepo.DeleteByUserID(ctx, user.ID)

	// Create new token with 1 hour expiry
	_, err = s.resetRepo.Create(ctx, sqlc.CreatePasswordResetTokenParams{
		UserID:    user.ID,
		Token:     token,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(1 * time.Hour), Valid: true},
	})
	if err != nil {
		return apperror.NewInternal("failed to create reset token")
	}

	// Set rate limit
	_ = s.cache.Set(ctx, cacheKey, []byte("1"), 1*time.Minute)

	// Send email
	resetURL := fmt.Sprintf("%s/reset-password?token=%s", s.frontendURL, token)
	if err := s.emailSender.Send(ctx, email.Message{
		To:      []string{user.Email},
		Subject: "Password Reset Request",
		HTML:    fmt.Sprintf("<p>Click <a href=%q>here</a> to reset your password. This link expires in 1 hour.</p>", resetURL),
	}); err != nil {
		slog.Error("failed to send password reset email", slog.Any("error", err))
	}

	return nil
}

func (s *passwordResetService) ResetPassword(ctx context.Context, req dto.ResetPasswordRequest) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
	if err != nil {
		return apperror.NewInternal("failed to hash password")
	}

	doReset := func(userRepo repository.UserRepository, resetRepo repository.PasswordResetRepository, refreshRepo repository.RefreshTokenRepository, forUpdate bool) error {
		var rt *sqlc.PasswordResetToken
		var err error
		if forUpdate {
			rt, err = resetRepo.GetByTokenForUpdate(ctx, req.Token)
		} else {
			rt, err = resetRepo.GetByToken(ctx, req.Token)
		}
		if err != nil {
			if errors.Is(err, apperror.ErrNotFound) {
				return apperror.NewBadRequest("invalid or expired reset token")
			}
			return apperror.NewInternal("failed to verify reset token")
		}

		if rt.ExpiresAt.Time.Before(time.Now()) {
			if err := resetRepo.Delete(ctx, req.Token); err != nil {
				slog.Error("failed to delete expired reset token", slog.Any("error", err))
			}
			return apperror.NewBadRequest("reset token has expired")
		}

		_, err = userRepo.UpdatePassword(ctx, sqlc.UpdateUserPasswordParams{
			PasswordHash: pgtype.Text{String: string(hash), Valid: true},
			ID:           rt.UserID,
		})
		if err != nil {
			return apperror.NewInternal("failed to update password")
		}
		if err := resetRepo.Delete(ctx, req.Token); err != nil {
			return apperror.NewInternal("failed to delete reset token")
		}
		if err := refreshRepo.DeleteByUserID(ctx, rt.UserID); err != nil {
			return apperror.NewInternal("failed to revoke refresh tokens")
		}
		return nil
	}

	if s.txManager != nil {
		return s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return doReset(
				repository.NewUserRepository(tx),
				repository.NewPasswordResetRepository(tx),
				repository.NewRefreshTokenRepository(tx),
				true,
			)
		})
	}

	return doReset(s.userRepo, s.resetRepo, s.refreshRepo, false)
}
