package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

type EmailVerificationService interface {
	SendVerification(ctx context.Context, userID int64, userEmail string) error
	Verify(ctx context.Context, token string) error
	ResendVerification(ctx context.Context, emailAddr string) error
}

type emailVerificationService struct {
	userRepo  repository.UserRepository
	verifRepo repository.EmailVerificationRepository
	sender    email.Sender
	cache     cache.Cache
	frontURL  string
}

func NewEmailVerificationService(
	userRepo repository.UserRepository,
	verifRepo repository.EmailVerificationRepository,
	sender email.Sender,
	appCache cache.Cache,
	frontendURL string,
) EmailVerificationService {
	return &emailVerificationService{
		userRepo:  userRepo,
		verifRepo: verifRepo,
		sender:    sender,
		cache:     appCache,
		frontURL:  frontendURL,
	}
}

func (s *emailVerificationService) SendVerification(ctx context.Context, userID int64, userEmail string) error {
	// Generate token
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generate verification token: %w", err)
	}
	token := hex.EncodeToString(b)

	// Delete old tokens
	_ = s.verifRepo.DeleteByUserID(ctx, userID)

	// Create with 24 hour expiry
	_, err := s.verifRepo.Create(ctx, sqlc.CreateEmailVerificationTokenParams{
		UserID:    userID,
		Token:     token,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("create verification token: %w", err)
	}

	// Send email — return error so callers (e.g. ResendVerification) can propagate it.
	// Fire-and-forget callers like the auth handler already ignore the return value.
	verifyURL := fmt.Sprintf("%s/verify-email?token=%s", s.frontURL, token)
	if err := s.sender.Send(ctx, email.Message{
		To:      []string{userEmail},
		Subject: "Verify Your Email Address",
		HTML:    fmt.Sprintf("<p>Click <a href=%q>here</a> to verify your email address. This link expires in 24 hours.</p>", verifyURL),
	}); err != nil {
		slog.Error("failed to send verification email",
			slog.Int64("user_id", userID),
			slog.String("email", userEmail),
			slog.Any("error", err),
		)
		return fmt.Errorf("send verification email: %w", err)
	}

	return nil
}

func (s *emailVerificationService) Verify(ctx context.Context, token string) error {
	vt, err := s.verifRepo.GetByToken(ctx, token)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewBadRequest("invalid or expired verification token")
		}
		return apperror.NewInternal("failed to verify token")
	}

	if vt.ExpiresAt.Time.Before(time.Now()) {
		_ = s.verifRepo.Delete(ctx, token)
		return apperror.NewBadRequest("verification token has expired")
	}

	// Mark email as verified
	_, err = s.userRepo.VerifyEmail(ctx, vt.UserID)
	if err != nil {
		return apperror.NewInternal("failed to verify email")
	}

	// Delete token
	_ = s.verifRepo.Delete(ctx, token)

	return nil
}

func (s *emailVerificationService) ResendVerification(ctx context.Context, emailAddr string) error {
	// Rate limit
	cacheKey := "email_verification:" + emailAddr
	exists, _ := s.cache.Exists(ctx, cacheKey)
	if exists {
		return apperror.NewBadRequest("please wait before requesting another verification email")
	}

	user, err := s.userRepo.GetByEmail(ctx, emailAddr)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil // Silent fail to prevent enumeration
		}
		return apperror.NewInternal("failed to process request")
	}

	// Skip if already verified
	if user.EmailVerifiedAt.Valid {
		return nil
	}

	_ = s.cache.Set(ctx, cacheKey, []byte("1"), 1*time.Minute)

	return s.SendVerification(ctx, user.ID, user.Email)
}
