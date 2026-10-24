package service

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// Google sign-in outcomes recorded in the auth.google_login audit metadata.
const (
	googleAccountExisting = "existing"
	googleAccountLinked   = "linked"
	googleAccountCreated  = "created"
)

// GoogleSignIn is one verified Google identity from the OAuth callback.
type GoogleSignIn struct {
	GoogleID string
	Email    string
	Name     string
	// AllowSignUp permits creating a new account (the user_registration
	// flag). An existing or linkable account may always sign in; otherwise a
	// sign-up attempt fails with feature.disabled.
	AllowSignUp bool
	// Complete runs inside the sign-in transaction after the account is
	// resolved and before auth.google_login is recorded. The caller applies
	// its admission rules (tenant, email verification) and issues the session
	// through tx. An error rolls back any account link/creation and writes no
	// audit row, so a refused sign-in is never audited as a success. tx is nil
	// when the service runs without a transaction manager. A nil Complete
	// admits every resolved account.
	Complete func(ctx context.Context, tx pgx.Tx, user *dto.UserResponse) error
}

// googleAccountResolver finds, links, or creates the account for a Google
// identity and names the outcome (googleAccount*).
type googleAccountResolver func(ctx context.Context, repo repository.UserRepository, req GoogleSignIn) (*identitysqlc.User, string, error)

func (s *userService) SignInWithGoogle(ctx context.Context, req GoogleSignIn) (*dto.UserResponse, error) {
	req.Email = NormalizeEmail(req.Email)
	user, err := s.completeGoogleSignIn(ctx, req, s.linkOrCreateGoogleUser)
	if err != nil {
		user, err = s.retryAfterConcurrentSignUp(ctx, req, err)
	}
	if err != nil {
		return nil, err
	}
	return ToUserResponse(user), nil
}

// completeGoogleSignIn resolves the account, lets the caller admit it and
// issue the session, and records the Tier 1 audit row in one transaction:
// the account change, the session, and the audit row commit together.
func (s *userService) completeGoogleSignIn(ctx context.Context, req GoogleSignIn, resolve googleAccountResolver) (*identitysqlc.User, error) {
	var user *identitysqlc.User
	err := inTx(ctx, s.txManager, s.fallbackRepos(), func(tx pgx.Tx, r repos) error {
		resolved, account, err := resolve(ctx, r.users, req)
		if err != nil {
			return err
		}
		if req.Complete != nil {
			if err := req.Complete(ctx, tx, ToUserResponse(resolved)); err != nil {
				return err
			}
		}
		if err := s.recordGoogleLogin(ctx, tx, resolved, account); err != nil {
			return err
		}
		user = resolved
		return nil
	})
	return user, err
}

// linkOrCreateGoogleUser signs in the account bound to the Google ID, links
// a verified local account with the same address, or creates a new account.
func (s *userService) linkOrCreateGoogleUser(ctx context.Context, repo repository.UserRepository, req GoogleSignIn) (*identitysqlc.User, string, error) {
	user, err := repo.GetByGoogleID(ctx, req.GoogleID)
	if err == nil {
		return user, googleAccountExisting, nil
	}
	if !errors.Is(err, apperror.ErrNotFound) {
		return nil, "", apperror.NewInternal("failed to find user by google id")
	}

	existing, err := repo.GetByEmail(ctx, req.Email)
	if err == nil {
		linked, linkErr := linkGoogleAccount(ctx, repo, existing, req.GoogleID)
		return linked, googleAccountLinked, linkErr
	}
	if !errors.Is(err, apperror.ErrNotFound) {
		return nil, "", apperror.NewInternal("failed to find user by email")
	}

	created, err := createGoogleUser(ctx, repo, req)
	return created, googleAccountCreated, err
}

// linkGoogleAccount binds googleID to an existing local account.
func linkGoogleAccount(ctx context.Context, repo repository.UserRepository, existing *identitysqlc.User, googleID string) (*identitysqlc.User, error) {
	// Never silently attach a Google identity to an unverified local account.
	// An attacker could otherwise pre-register a victim's address and take
	// over the account when the victim uses Google.
	if !existing.EmailVerifiedAt.Valid {
		return nil, apperror.NewConflict("an account with this email already exists; verify it before linking Google")
	}
	linked, err := repo.LinkGoogleAccount(ctx, identitysqlc.LinkGoogleAccountParams{
		GoogleID: pgtype.Text{String: googleID, Valid: true},
		ID:       existing.ID,
	})
	switch {
	case err == nil:
		return linked, nil
	case database.IsUniqueViolation(err):
		return nil, apperror.NewConflict("this Google account is already linked to another user")
	case errors.Is(err, apperror.ErrNotFound):
		// The guarded UPDATE matched no row: the account is already bound to
		// a different Google identity, which is never replaced.
		return nil, apperror.NewConflict("this account is already linked to a different Google account")
	default:
		return nil, apperror.NewInternal("failed to link google account")
	}
}

// createGoogleUser registers a new Google account. A unique violation is
// returned unwrapped so SignInWithGoogle can detect a concurrent sign-up.
func createGoogleUser(ctx context.Context, repo repository.UserRepository, req GoogleSignIn) (*identitysqlc.User, error) {
	if !req.AllowSignUp {
		return nil, apperror.NewCode(apperror.CodeFeatureDisabled, "feature user_registration is disabled")
	}
	if err := ensureEmailNotBanned(ctx, repo, req.Email); err != nil {
		return nil, err
	}
	return repo.CreateOAuthUser(ctx, identitysqlc.CreateOAuthUserParams{
		TenantID:     tenant.EffectiveID(ctx),
		Email:        req.Email,
		Name:         req.Name,
		GoogleID:     pgtype.Text{String: req.GoogleID, Valid: true},
		AuthProvider: "google",
	})
}

// retryAfterConcurrentSignUp handles the concurrent-create race. When another
// request created the account first (unique violation), this request signs
// in to that account with its own admission check and audit row. Any other
// AppError is returned as is; anything else becomes a generic 500.
func (s *userService) retryAfterConcurrentSignUp(ctx context.Context, req GoogleSignIn, cause error) (*identitysqlc.User, error) {
	if !database.IsUniqueViolation(cause) {
		var appErr *apperror.AppError
		if errors.As(cause, &appErr) {
			return nil, cause
		}
		return nil, apperror.NewInternal("failed to create oauth user")
	}
	winner, err := s.repo.GetByGoogleID(ctx, req.GoogleID)
	if err != nil {
		if _, emailErr := s.repo.GetByEmail(ctx, req.Email); emailErr == nil {
			return nil, apperror.NewConflict("an account with this email already exists; verify it before linking Google")
		}
		return nil, apperror.NewInternal("failed to create oauth user")
	}
	return s.completeGoogleSignIn(ctx, req, func(context.Context, repository.UserRepository, GoogleSignIn) (*identitysqlc.User, string, error) {
		return winner, googleAccountExisting, nil
	})
}

// recordGoogleLogin writes the Tier 1 auth.google_login event. The OAuth
// callback skips tenant middleware (its tenant comes from the signed state),
// so the event takes its tenant and actor from the signed-in account rather
// than from request-level audit state.
func (s *userService) recordGoogleLogin(ctx context.Context, tx pgx.Tx, user *identitysqlc.User, account string) error {
	event := audit.NewServiceEvent(ctx, "auth.google_login", "auth")
	event.TenantID = user.TenantID
	event.ResourceID = strconv.FormatInt(user.ID, 10)
	event.ActorID = user.ID
	event.ActorRole = user.Role
	event.ActorType = audit.ActorTypeForRole(user.Role)
	if event.ActorType == audit.ActorTypeUnknown {
		event.ActorType = audit.ActorTypeUser
	}
	event.Metadata = map[string]any{
		"account":        account,
		"email_verified": user.EmailVerifiedAt.Valid,
	}
	return checkAudit(audit.RecordInTx(ctx, s.audit, tx, event))
}
