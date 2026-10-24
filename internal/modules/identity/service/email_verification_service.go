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

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
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

// VerificationProvisioner creates a verification token and enqueues its email
// on a caller-owned transaction. UserService uses it during registration so
// the user, token, and email job commit together.
type VerificationProvisioner interface {
	SendVerificationTx(ctx context.Context, tx pgx.Tx, userID int64, userEmail string) error
	// SupportsTransactionalEnqueue reports whether SendVerificationTx can
	// enqueue the email on the caller's transaction.
	SupportsTransactionalEnqueue() bool
}

type EmailVerificationService interface {
	SendVerification(ctx context.Context, userID int64, userEmail string) error
	SendVerificationTx(ctx context.Context, tx pgx.Tx, userID int64, userEmail string) error
	SupportsTransactionalEnqueue() bool
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
	// tenants is optional (tests/minimal deployments). Production supplies it
	// so links built without a request slug still carry the right tenant.
	tenants repository.TenantRepository
}

func NewEmailVerificationService(
	userRepo repository.UserRepository,
	verifRepo repository.EmailVerificationRepository,
	sender email.Sender,
	appCache cache.Cache,
	frontendURL string,
	txManager *database.TxManager,
	tenants ...repository.TenantRepository,
) EmailVerificationService {
	var tenantRepo repository.TenantRepository
	if len(tenants) > 0 {
		tenantRepo = tenants[0]
	}
	return &emailVerificationService{
		userRepo:  userRepo,
		verifRepo: verifRepo,
		sender:    sender,
		cache:     appCache,
		frontURL:  frontendURL,
		txManager: txManager,
		tenants:   tenantRepo,
	}
}

// SupportsTransactionalEnqueue reports whether this service can write the
// token and enqueue its email on a caller-owned transaction.
func (s *emailVerificationService) SupportsTransactionalEnqueue() bool {
	if s.txManager == nil {
		return false
	}
	_, transactional := s.sender.(transactionalEmailSender)
	return transactional
}

func (s *emailVerificationService) SendVerification(ctx context.Context, userID int64, userEmail string) error {
	userEmail = NormalizeEmail(userEmail)
	msg, tokenHash, err := s.buildVerificationMessage(ctx, userEmail)
	if err != nil {
		return err
	}
	txSender, transactional := s.sender.(transactionalEmailSender)

	if s.txManager != nil {
		if err := s.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			return s.persistVerification(
				ctx, repository.NewEmailVerificationRepository(tx), tx, txSender, userID, userEmail, tokenHash, msg,
			)
		}); err != nil {
			return err
		}
	} else if err := s.persistVerification(ctx, s.verifRepo, nil, nil, userID, userEmail, tokenHash, msg); err != nil {
		return err
	}

	if s.txManager != nil && transactional {
		// persistVerification enqueued the message on the token transaction.
		return nil
	}

	// Send email — return error so callers (e.g. ResendVerification) can
	// propagate it. Fire-and-forget callers like the auth handler already
	// ignore the return value.
	if err := s.sender.Send(ctx, msg); err != nil {
		slog.Error("failed to send verification email",
			slog.Int64("user_id", userID),
			slog.Any("error", err),
		)
		return fmt.Errorf("send verification email: %w", err)
	}

	return nil
}

// SendVerificationTx creates the verification token and enqueues its email on
// tx, so the caller can commit both with the state change that triggered them.
// It fails closed: a nil transaction or a non-transactional sender is an error
// rather than a silent post-commit send.
func (s *emailVerificationService) SendVerificationTx(ctx context.Context, tx pgx.Tx, userID int64, userEmail string) error {
	// Check sender capability first: a non-transactional sender can never honor
	// this method, regardless of whether the transaction is present.
	txSender, transactional := s.sender.(transactionalEmailSender)
	if !transactional {
		return errors.New("send verification: sender does not support transactional enqueue")
	}
	if tx == nil {
		return errors.New("send verification: transaction is required")
	}

	userEmail = NormalizeEmail(userEmail)
	msg, tokenHash, err := s.buildVerificationMessage(ctx, userEmail)
	if err != nil {
		return err
	}

	return s.persistVerification(
		ctx, repository.NewEmailVerificationRepository(tx), tx, txSender, userID, userEmail, tokenHash, msg,
	)
}

// buildVerificationMessage generates a fresh verification token and the email
// that delivers it. Only the returned hash is ever persisted.
func (s *emailVerificationService) buildVerificationMessage(ctx context.Context, userEmail string) (email.Message, string, error) {
	// Generate token
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return email.Message{}, "", fmt.Errorf("generate verification token: %w", err)
	}
	token := hex.EncodeToString(b)

	linkCtx, err := s.withLinkTenantSlug(ctx)
	if err != nil {
		return email.Message{}, "", err
	}
	verifyURL := tokenLink(linkCtx, s.frontURL, "verify-email", token)
	msg := email.Message{
		To:      []string{userEmail},
		Subject: "Verify Your Email Address",
		HTML:    fmt.Sprintf("<p>Click <a href=%q>here</a> to verify your email address. This link expires in 24 hours.</p>", verifyURL),
	}
	return msg, hashToken(token), nil
}

// withLinkTenantSlug makes sure the link carries the slug of the tenant the
// token is written to. The request slug is absent when an authenticated
// credential's tenant overrode a header-less default resolution (JWTAuth and
// the API-key middleware clear it); a link without a slug would then be
// resolved against the default tenant and never verify. In that case the slug
// is looked up from the token's tenant ID.
func (s *emailVerificationService) withLinkTenantSlug(ctx context.Context) (context.Context, error) {
	if tenant.SlugFromContext(ctx) != "" || s.tenants == nil {
		return ctx, nil
	}
	found, err := s.tenants.GetByIDIncludingSuspended(ctx, tenant.EffectiveID(ctx))
	if err != nil {
		return nil, fmt.Errorf("resolve tenant slug for verification link: %w", err)
	}
	return tenant.WithSlug(ctx, found.Slug), nil
}

// persistVerification replaces any pending token and, when tx is present,
// enqueues the verification email on the same transaction.
func (s *emailVerificationService) persistVerification(
	ctx context.Context,
	verifRepo repository.EmailVerificationRepository,
	tx pgx.Tx,
	txSender transactionalEmailSender,
	userID int64,
	userEmail string,
	tokenHash string,
	msg email.Message,
) error {
	// Delete old tokens first so a user has at most one live link.
	if err := verifRepo.DeleteByUserID(ctx, userID); err != nil {
		return fmt.Errorf("delete old verification tokens: %w", err)
	}

	// Create with 24 hour expiry. Store only the hash; the plaintext token
	// is delivered to the user via email so a DB leak cannot be replayed.
	_, err := verifRepo.Create(ctx, identitysqlc.CreateEmailVerificationTokenParams{
		TenantID:  tenant.EffectiveID(ctx),
		UserID:    userID,
		Email:     userEmail,
		Token:     tokenHash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("create verification token: %w", err)
	}

	if tx != nil && txSender != nil {
		// Enqueue on the same transaction: a crash between the token insert
		// and the enqueue cannot leave a usable verification link without
		// an email.
		if err := txSender.SendTx(ctx, tx, msg); err != nil {
			slog.Error("failed to enqueue verification email",
				slog.Int64("user_id", userID),
				slog.Any("error", err),
			)
			return fmt.Errorf("send verification email: %w", err)
		}
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
	if vt.TenantID != tenant.EffectiveID(ctx) {
		return apperror.NewBadRequest("invalid or expired verification token")
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
	if NormalizeEmail(user.Email) != NormalizeEmail(vt.Email) {
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
	emailAddr = NormalizeEmail(emailAddr)

	// Rate limit before user lookup so the response cannot reveal whether the
	// address exists. Increment is atomic and sets the TTL on first use.
	cacheKey := scopedKey(ctx, "email_verification:", emailAddr)
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
	tenantSlug := tenant.SlugFromContext(ctx)
	async.Go(func() {
		// The fire-and-forget goroutine does not inherit the request context.
		// Carry the user's tenant (and resolved slug for the email link) so
		// token writes and browser navigation stay scoped.
		sendCtx := tenant.WithIdentity(context.Background(), user.TenantID, tenantSlug)
		if err := s.SendVerification(sendCtx, user.ID, user.Email); err != nil {
			slog.Error("failed to resend verification email",
				slog.Int64("user_id", user.ID),
				slog.Any("error", err),
			)
		}
	})
	return nil
}

func (s *emailVerificationService) Invalidate(ctx context.Context, userID int64) error {
	return s.verifRepo.DeleteByUserID(ctx, userID)
}
