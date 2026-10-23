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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/email"
)

// transactionalEmailSender can enqueue an email on the caller's database
// transaction. jobs.QueuedSender implements it; plain senders only implement
// email.Sender and are sent after the token transaction commits.
type transactionalEmailSender interface {
	SendTx(ctx context.Context, tx pgx.Tx, msg email.Message) error
}

type EmailVerificationService interface {
	SendVerification(ctx context.Context, userID int64, userEmail string) error
	Verify(ctx context.Context, token string) error
	ResendVerification(ctx context.Context, emailAddr string) error
	// Invalidate removes pending verification tokens, e.g. after an email
	// address change so a link sent to the old address cannot verify the new one.
	Invalidate(ctx context.Context, userID int64) error
}

type emailVerificationService struct {
	userRepo  repository.UserRepository
	verifRepo repository.EmailVerificationRepository
	sender    email.Sender
	cache     cache.Cache
	frontURL  string
	txManager *database.TxManager
}

func NewEmailVerificationService(
	userRepo repository.UserRepository,
	verifRepo repository.EmailVerificationRepository,
	sender email.Sender,
	appCache cache.Cache,
	frontendURL string,
	txManager *database.TxManager,
) EmailVerificationService {
	return &emailVerificationService{
		userRepo:  userRepo,
		verifRepo: verifRepo,
		sender:    sender,
		cache:     appCache,
		frontURL:  frontendURL,
		txManager: txManager,
	}
}

func (s *emailVerificationService) SendVerification(ctx context.Context, userID int64, userEmail string) error {
	userEmail = normalizeEmail(userEmail)

	// Generate token
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generate verification token: %w", err)
	}
	token := hex.EncodeToString(b)

	verifyURL := fmt.Sprintf("%s/verify-email?token=%s", s.frontURL, token)
	msg := email.Message{
		To:      []string{userEmail},
		Subject: "Verify Your Email Address",
		HTML:    fmt.Sprintf("<p>Click <a href=%q>here</a> to verify your email address. This link expires in 24 hours.</p>", verifyURL),
	}
	txSender, transactional := s.sender.(transactionalEmailSender)

	queuedInTx := false
	createToken := func(verifRepo repository.EmailVerificationRepository, tx pgx.Tx) error {
		// Delete old tokens first so a user has at most one live link.
		if err := verifRepo.DeleteByUserID(ctx, userID); err != nil {
			return fmt.Errorf("delete old verification tokens: %w", err)
		}

		// Create with 24 hour expiry. Store only the hash; the plaintext token
		// is delivered to the user via email so a DB leak cannot be replayed.
		_, err := verifRepo.Create(ctx, sqlc.CreateEmailVerificationTokenParams{
			UserID:    userID,
			Email:     userEmail,
			Token:     hashToken(token),
			ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("create verification token: %w", err)
		}

		if tx != nil && transactional {
			// Enqueue on the same transaction: a crash between the token insert
			// and the enqueue cannot leave a usable verification link without
			// an email.
			if err := txSender.SendTx(ctx, tx, msg); err != nil {
				slog.Error("failed to enqueue verification email",
					slog.Int64("user_id", userID),
					slog.String("email", userEmail),
					slog.Any("error", err),
				)
				return fmt.Errorf("send verification email: %w", err)
			}
			queuedInTx = true
		}
		return nil
	}

	if s.txManager != nil {
		if err := s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return createToken(repository.NewEmailVerificationRepository(tx), tx)
		}); err != nil {
			return err
		}
	} else if err := createToken(s.verifRepo, nil); err != nil {
		return err
	}

	if queuedInTx {
		return nil
	}

	// Send email — return error so callers (e.g. ResendVerification) can
	// propagate it. Fire-and-forget callers like the auth handler already
	// ignore the return value.
	if err := s.sender.Send(ctx, msg); err != nil {
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
	tokenHash := hashToken(token)

	// Consume atomically so a verification link cannot be replayed by two
	// concurrent requests (or reused after a successful verification).
	vt, err := s.verifRepo.Consume(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewBadRequest("invalid or expired verification token")
		}
		return apperror.NewInternal("failed to verify token")
	}

	if vt.ExpiresAt.Time.Before(time.Now()) {
		return apperror.NewBadRequest("verification token has expired")
	}

	// The link proves ownership of the address it was sent to, not merely of
	// the account. Reject it if the account has since changed address.
	user, err := s.userRepo.GetByID(ctx, vt.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewBadRequest("invalid or expired verification token")
		}
		return apperror.NewInternal("failed to load user")
	}
	if normalizeEmail(user.Email) != normalizeEmail(vt.Email) {
		return apperror.NewBadRequest("verification token does not match the current email")
	}

	// Mark email as verified
	if _, err := s.userRepo.VerifyEmail(ctx, vt.UserID, vt.Email); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewBadRequest("verification token does not match the current email")
		}
		return apperror.NewInternal("failed to verify email")
	}

	return nil
}

func (s *emailVerificationService) ResendVerification(ctx context.Context, emailAddr string) error {
	emailAddr = normalizeEmail(emailAddr)

	// Rate limit before user lookup so the response cannot reveal whether the
	// address exists. Increment is atomic and sets the TTL on first use.
	cacheKey := "email_verification:" + emailAddr
	if s.cache != nil {
		if n, err := s.cache.Increment(ctx, cacheKey, 1*time.Minute); err == nil && n > 1 {
			return nil
		}
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

	// Send asynchronously so response timing does not reveal whether the
	// address exists or whether a message was actually delivered.
	async.Go(func() {
		if err := s.SendVerification(context.Background(), user.ID, user.Email); err != nil {
			slog.Error("failed to resend verification email",
				slog.Int64("user_id", user.ID),
				slog.String("email", user.Email),
				slog.Any("error", err),
			)
		}
	})
	return nil
}

func (s *emailVerificationService) Invalidate(ctx context.Context, userID int64) error {
	return s.verifRepo.DeleteByUserID(ctx, userID)
}
