package authn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/email"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// verificationTokenTTL is how long an emailed verification link stays valid.
const verificationTokenTTL = 24 * time.Hour

// EmailVerificationService verifies a user's (global) email address. Tokens
// are per user and bound to the address they were sent to; the emailed link
// carries the request tenant's slug only so the frontend knows where the user
// signs in.
type EmailVerificationService interface {
	SendVerification(ctx context.Context, userID uuid.UUID, userEmail string) error
	Verify(ctx context.Context, token string) error
	ResendVerification(ctx context.Context, emailAddr string) error
	// Invalidate removes pending verification tokens, e.g. after an email
	// address change so a link sent to the old address cannot verify the new one.
	Invalidate(ctx context.Context, userID uuid.UUID) error
}

// txProvisioner is the transactional side of EmailVerificationService:
// registration and invitation sign-up write the token and enqueue its email
// on their own transaction.
type txProvisioner interface {
	provisionTx(ctx context.Context, tx TxRepos, userID uuid.UUID, userEmail string) error
}

// VerificationDeps are the email verification service's collaborators.
type VerificationDeps struct {
	Stores      Stores
	Tx          Transactor
	Cache       cache.Cache
	FrontendURL string
}

type emailVerificationService struct {
	VerificationDeps
}

// NewEmailVerificationService wires the verification service.
func NewEmailVerificationService(deps VerificationDeps) EmailVerificationService {
	return &emailVerificationService{VerificationDeps: deps}
}

func (s *emailVerificationService) SendVerification(ctx context.Context, userID uuid.UUID, userEmail string) error {
	userEmail = NormalizeEmail(userEmail)
	msg, tokenHash, err := s.buildVerificationMessage(ctx, userEmail)
	if err != nil {
		return err
	}

	// The token and its email job commit together; there is no out-of-band
	// send to duplicate or lose.
	return s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		return persistVerification(ctx, tx, userID, userEmail, tokenHash, msg)
	})
}

// provisionTx creates the verification token and enqueues its email on tx,
// so the caller can commit both with the state change that triggered them.
// It fails closed: a transaction without an email outbox is an error rather
// than a silent post-commit send.
func (s *emailVerificationService) provisionTx(ctx context.Context, tx TxRepos, userID uuid.UUID, userEmail string) error {
	if tx.Mail == nil {
		return errors.New("send verification: the transaction has no email outbox")
	}
	userEmail = NormalizeEmail(userEmail)
	msg, tokenHash, err := s.buildVerificationMessage(ctx, userEmail)
	if err != nil {
		return err
	}
	return persistVerification(ctx, tx, userID, userEmail, tokenHash, msg)
}

// buildVerificationMessage generates a fresh verification token and the email
// that delivers it. Only the returned hash is ever persisted.
func (s *emailVerificationService) buildVerificationMessage(ctx context.Context, userEmail string) (email.Message, []byte, error) {
	token, tokenHash, err := newOpaqueToken()
	if err != nil {
		return email.Message{}, nil, fmt.Errorf("generate verification token: %w", err)
	}
	linkCtx, err := withLinkTenantSlug(ctx, s.Stores.Tenants)
	if err != nil {
		return email.Message{}, nil, err
	}
	verifyURL := tokenLink(linkCtx, s.FrontendURL, "verify-email", token)
	msg := email.Message{
		To:      []string{userEmail},
		Subject: "Verify Your Email Address",
		HTML:    fmt.Sprintf("<p>Click <a href=%q>here</a> to verify your email address. This link expires in 24 hours.</p>", verifyURL),
	}
	return msg, tokenHash, nil
}

// persistVerification replaces any pending token and, when the transaction
// has an email outbox, enqueues the verification email on it: a crash
// between the token insert and the enqueue cannot leave a usable link
// without an email.
func persistVerification(ctx context.Context, tx TxRepos, userID uuid.UUID, userEmail string, tokenHash []byte, msg email.Message) error {
	if tx.Mail == nil {
		return errors.New("send verification: the transaction has no email outbox")
	}
	if err := tx.Verifications.Create(ctx, userID, userEmail, tokenHash, time.Now().Add(verificationTokenTTL)); err != nil {
		return fmt.Errorf("create verification token: %w", err)
	}
	if err := tx.Mail.Send(ctx, msg); err != nil {
		slog.Error("failed to enqueue verification email",
			slog.String("user_id", userID.String()),
			slog.Any("error", err),
		)
		return fmt.Errorf("send verification email: %w", err)
	}
	return nil
}

func invalidVerificationToken() error {
	return apperror.NewBadRequest("invalid or expired verification token")
}

func (s *emailVerificationService) Verify(ctx context.Context, token string) error {
	// Consume atomically so a verification link cannot be replayed by two
	// concurrent requests (or reused after a successful verification).
	vt, err := s.Stores.Verifications.Consume(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return invalidVerificationToken()
		}
		return apperror.NewInternal("failed to verify token")
	}
	if !vt.ExpiresAt.After(time.Now()) {
		return apperror.NewBadRequest("verification token has expired")
	}

	// The link proves ownership of the address it was sent to, not merely of
	// the account. Reject it if the account has since changed address.
	user, err := s.Stores.Users.GetByID(ctx, vt.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return invalidVerificationToken()
		}
		return apperror.NewInternal("failed to load user")
	}
	if user.Email != NormalizeEmail(vt.Email) {
		return apperror.NewBadRequest("verification token does not match the current email")
	}
	if _, err := s.Stores.Users.VerifyEmail(ctx, vt.UserID, vt.Email); err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewBadRequest("verification token does not match the current email")
		}
		return apperror.NewInternal("failed to verify email")
	}
	return nil
}

func (s *emailVerificationService) ResendVerification(ctx context.Context, emailAddr string) error {
	emailAddr = NormalizeEmail(emailAddr)

	// Rate limit before the lookup so the response cannot reveal whether the
	// address exists. Increment is atomic and sets the TTL on first use.
	if s.Cache != nil {
		if n, err := s.Cache.Increment(ctx, emailKey("email_verification:", emailAddr), time.Minute); err == nil && n > 1 {
			return nil
		}
	}

	user, err := s.Stores.Users.GetByEmail(ctx, emailAddr)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil // Silent to prevent enumeration.
		}
		return apperror.NewInternal("failed to process request")
	}
	if user.EmailVerified() || user.Status != UserStatusActive {
		return nil
	}

	// Enqueue the email job in the request tenant's transaction. The address
	// was already looked up silently, so a failure stays logged and the
	// endpoint keeps the same response for known and unknown accounts.
	if err := s.SendVerification(ctx, user.ID, user.Email); err != nil {
		slog.Error("failed to resend verification email",
			slog.String("user_id", user.ID.String()),
			slog.Any("error", err),
		)
	}
	return nil
}

func (s *emailVerificationService) Invalidate(ctx context.Context, userID uuid.UUID) error {
	return s.Stores.Verifications.DeleteByUser(ctx, userID)
}

// EmailVerificationServiceImpl is the concrete verification service, exported
// for composition and white-box tests outside this package.
type EmailVerificationServiceImpl = emailVerificationService
