package service

import (
	"context"
	"errors"
	"strconv"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Google sign-in outcomes recorded in the auth.google_login audit metadata.
const (
	googleAccountExisting = "existing"
	googleAccountLinked   = "linked"
	googleAccountCreated  = "created"
)

// GoogleSignIn is one verified Google identity from the OAuth callback, for
// the request tenant (the tenant bound into the signed OAuth state).
type GoogleSignIn struct {
	GoogleID string
	Email    string
	Name     string
	// EmailVerified is Google's verified_email. An unverified Google address
	// can neither create an account nor link to one.
	EmailVerified bool
	// AllowSignUp permits creating a new account (the user_registration
	// flag). An existing or linkable account may always sign in; otherwise a
	// sign-up attempt fails with feature.disabled.
	AllowSignUp bool
	// Complete runs inside the sign-in transaction after the account and its
	// membership are resolved and before the session is issued. The caller
	// applies its admission rules (email verification flag). An error rolls
	// back any account link/creation and writes no audit row, so a refused
	// sign-in is never audited as a success. A nil Complete admits every
	// resolved member.
	Complete func(ctx context.Context, user *dto.UserResponse) error
	// MFARequired runs inside the same transaction after Complete. When it
	// reports true, the first factor succeeded but no session is issued; the
	// caller starts a two-step login challenge. A nil hook never requires MFA.
	MFARequired func(ctx context.Context, member *Member) (bool, error)
}

// GoogleSignInResult is the signed-in member and the issued session. When
// MFARequired is set, Tokens is empty and the caller must start a challenge.
type GoogleSignInResult struct {
	User        *dto.UserResponse
	Tokens      TokenPair
	MFARequired bool
}

// SignInWithGoogle resolves the Google identity to a global user (existing
// link, a verified local account with the same address, or a new account),
// requires an active membership in the request tenant (a new account gets
// one; an existing user joins other tenants through invitations), and issues
// the session and the Tier 1 auth.google_login audit row in one transaction.
func (s *userService) SignInWithGoogle(ctx context.Context, req GoogleSignIn) (*GoogleSignInResult, error) {
	req.Email = NormalizeEmail(req.Email)
	result, err := s.completeGoogleSignIn(ctx, req)
	if errors.Is(err, errConcurrentSignUp) {
		// Another request created the account or the link first; this
		// request signs in to it with its own admission check and audit row.
		result, err = s.completeGoogleSignIn(ctx, req)
	}
	if errors.Is(err, errConcurrentSignUp) {
		return nil, apperror.NewConflict("the account was modified concurrently, please retry")
	}
	return result, err
}

// errConcurrentSignUp aborts a sign-in transaction that lost a create/link
// race; SignInWithGoogle retries once.
var errConcurrentSignUp = errors.New("identity: concurrent google sign-up")

func (s *userService) completeGoogleSignIn(ctx context.Context, req GoogleSignIn) (*GoogleSignInResult, error) {
	var result *GoogleSignInResult
	err := s.Tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		member, account, err := s.resolveGoogleMember(ctx, tx.Stores, req)
		if err != nil {
			return err
		}
		user := ToUserResponse(member)
		if req.Complete != nil {
			if err := req.Complete(ctx, user); err != nil {
				return err
			}
		}
		mfaRequired := false
		if req.MFARequired != nil {
			required, err := req.MFARequired(ctx, member)
			if err != nil {
				return err
			}
			mfaRequired = required
		}
		var tokens TokenPair
		if !mfaRequired {
			tokens, err = s.Sessions.issueTx(ctx, tx.Stores, member.User.ID, member.User.TokenVersion, []string{AMRGoogle})
			if err != nil {
				return err
			}
		}
		if err := s.recordGoogleLogin(ctx, tx, member, account, mfaRequired); err != nil {
			return err
		}
		result = &GoogleSignInResult{User: user, Tokens: tokens, MFARequired: mfaRequired}
		return nil
	})
	return result, err
}

// resolveGoogleMember finds, links, or creates the user and returns its
// active membership in the request tenant plus the outcome (googleAccount*).
func (s *userService) resolveGoogleMember(ctx context.Context, st Stores, req GoogleSignIn) (*Member, string, error) {
	user, err := st.Users.GetByIdentity(ctx, ProviderGoogle, req.GoogleID)
	switch {
	case err == nil:
		member, memberErr := googleMembership(ctx, st, user)
		return member, googleAccountExisting, memberErr
	case !errors.Is(err, apperror.ErrNotFound):
		return nil, "", apperror.NewInternal("failed to find user by google id")
	}
	if !req.EmailVerified {
		return nil, "", apperror.NewBadRequest("the Google account email is not verified")
	}

	existing, err := st.Users.GetByEmail(ctx, req.Email)
	switch {
	case err == nil:
		if linkErr := linkGoogleAccount(ctx, st, existing, req); linkErr != nil {
			return nil, "", linkErr
		}
		member, memberErr := googleMembership(ctx, st, existing)
		return member, googleAccountLinked, memberErr
	case !errors.Is(err, apperror.ErrNotFound):
		return nil, "", apperror.NewInternal("failed to find user by email")
	}

	created, err := createGoogleAccount(ctx, st, req)
	return created, googleAccountCreated, err
}

// googleMembership requires an active membership of user in the request
// tenant. Signing in with Google never joins a tenant implicitly.
func googleMembership(ctx context.Context, st Stores, user *User) (*Member, error) {
	if user.Status != UserStatusActive {
		return nil, accountNotActive()
	}
	member, err := st.Memberships.GetActive(ctx, user.ID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, accountNotActive()
		}
		return nil, apperror.NewInternal("failed to load membership")
	}
	return member, nil
}

// linkGoogleAccount binds the Google identity to an existing local account.
func linkGoogleAccount(ctx context.Context, st Stores, existing *User, req GoogleSignIn) error {
	// Never silently attach a Google identity to an unverified local account.
	// An attacker could otherwise pre-register a victim's address and take
	// over the account when the victim uses Google.
	if !existing.EmailVerified() {
		return apperror.NewConflict("an account with this email already exists; verify it before linking Google")
	}
	err := st.Users.LinkIdentity(ctx, ExternalIdentity{
		UserID: existing.ID, Provider: ProviderGoogle, Subject: req.GoogleID, Email: req.Email,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrIdentityTaken):
		// A concurrent sign-in linked this Google identity first.
		return errConcurrentSignUp
	case errors.Is(err, ErrIdentityConflict):
		// The account is already bound to a different Google identity, which
		// is never replaced.
		return apperror.NewConflict("this account is already linked to a different Google account")
	default:
		return apperror.NewInternal("failed to link google account")
	}
}

// createGoogleAccount registers a new verified account linked to the Google
// identity, with a membership in the request tenant.
func createGoogleAccount(ctx context.Context, st Stores, req GoogleSignIn) (*Member, error) {
	if !req.AllowSignUp {
		return nil, apperror.NewCode(apperror.CodeFeatureDisabled, "feature user_registration is disabled")
	}
	if err := ensureEmailNotBanned(ctx, st.Memberships, req.Email); err != nil {
		return nil, err
	}
	member, err := createAccount(ctx, st, NewUser{Email: req.Email, Name: req.Name, EmailVerified: true}, "", RoleUser)
	if err != nil {
		var appErr *apperror.AppError
		if errors.As(err, &appErr) && appErr.Code == string(identitycatalog.CodeUserEmailTaken) {
			return nil, errConcurrentSignUp
		}
		return nil, err
	}
	linkErr := st.Users.LinkIdentity(ctx, ExternalIdentity{
		UserID: member.User.ID, Provider: ProviderGoogle, Subject: req.GoogleID, Email: req.Email,
	})
	switch {
	case linkErr == nil:
		return member, nil
	case errors.Is(linkErr, ErrIdentityTaken):
		return nil, errConcurrentSignUp
	default:
		return nil, apperror.NewInternal("failed to create oauth user")
	}
}

// recordGoogleLogin writes the Tier 1 auth.google_login event. The OAuth
// callback skips tenant middleware (its tenant comes from the signed state),
// so the event takes its tenant and actor from the signed-in member rather
// than from request-level audit state.
func (s *userService) recordGoogleLogin(ctx context.Context, tx TxRepos, member *Member, account string, mfaRequired bool) error {
	event := audit.NewServiceEvent(ctx, "auth.google_login", "auth")
	event.TenantID = member.Membership.TenantID
	event.ResourceID = strconv.FormatInt(member.User.ID, 10)
	event.ActorID = member.User.ID
	event.ActorRole = member.Membership.Role
	event.ActorType = audit.ActorTypeForRole(member.Membership.Role)
	if event.ActorType == audit.ActorTypeUnknown {
		event.ActorType = audit.ActorTypeUser
	}
	event.Metadata = map[string]any{
		"account":        account,
		"email_verified": member.User.EmailVerified(),
		"mfa_required":   mfaRequired,
	}
	return checkAudit(tx.Audit.RecordTx(ctx, event))
}
