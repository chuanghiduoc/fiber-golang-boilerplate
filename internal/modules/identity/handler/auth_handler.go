package handler

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/oauth2"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/cache"
	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

const oauthStateCookieName = "__Host-oauth_state"

const (
	// googleExchangeTTL is how long the callback's one-time code can be
	// redeemed.
	googleExchangeTTL = 60 * time.Second
	// googleExchangeCodeBytes makes the code 256 bits of entropy.
	googleExchangeCodeBytes = 32
	googleExchangeKeyPrefix = "oauth:google:exchange:"
)

// oauthStateKeyInfo domain-separates the OAuth state MAC key from every other
// use of the JWT secret.
const oauthStateKeyInfo = "fiber-golang-boilerplate/identity/oauth-state/v1"

// oauthStateKey derives the HMAC-SHA256 key for OAuth state from the JWT
// secret with HKDF, so the state MAC never reuses the token-signing key.
func oauthStateKey(jwtSecret string) []byte {
	key := make([]byte, sha256.Size)
	if _, err := io.ReadFull(hkdf.New(sha256.New, []byte(jwtSecret), nil, []byte(oauthStateKeyInfo)), key); err != nil {
		// HKDF-SHA256 can always produce 32 bytes; failure is a programming error.
		panic("identity: derive oauth state key: " + err.Error())
	}
	return key
}

// encodeOAuthState binds the OAuth state nonce to the tenant that started the
// flow and signs both: "<nonce>.<tenantID>.<hex HMAC>". The callback uses
// this tenant instead of X-Tenant-Slug, otherwise an attacker could start a
// flow for tenant A and complete it under tenant B.
func encodeOAuthState(key []byte, nonce string, tenantID uuid.UUID) string {
	payload := nonce + "." + tenantID.String()
	return payload + "." + oauthStateMAC(key, payload)
}

// decodeOAuthState verifies the MAC in constant time before trusting the
// tenant carried in state.
func decodeOAuthState(key []byte, state string) (nonce string, tenantID uuid.UUID, ok bool) {
	macSep := strings.LastIndexByte(state, '.')
	if macSep <= 0 || macSep == len(state)-1 {
		return "", uuid.Nil, false
	}
	payload, mac := state[:macSep], state[macSep+1:]
	if !hmac.Equal([]byte(mac), []byte(oauthStateMAC(key, payload))) {
		return "", uuid.Nil, false
	}
	sep := strings.LastIndexByte(payload, '.')
	if sep <= 0 || sep == len(payload)-1 {
		return "", uuid.Nil, false
	}
	id, err := uuid.Parse(payload[sep+1:])
	if err != nil || id == uuid.Nil {
		return "", uuid.Nil, false
	}
	return payload[:sep], id, true
}

func oauthStateMAC(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// oauthFlowMACPrefix domain-separates the cookie flow MAC from the state MAC.
const oauthFlowMACPrefix = "pkce-flow:"

// encodeOAuthFlow stores the signed state and the PKCE verifier in the state
// cookie. The verifier never leaves the server (it is not part of the state
// query parameter sent to Google); the cookie MAC binds it to that state so a
// tampered cookie cannot pair another verifier with the flow.
func encodeOAuthFlow(key []byte, state, verifier string) string {
	payload := state + "." + verifier
	return payload + "." + oauthStateMAC(key, oauthFlowMACPrefix+payload)
}

// decodeOAuthFlow returns the state and PKCE verifier carried by the callback
// cookie. A legacy cookie that holds only a signed state is accepted with an
// empty verifier: the provider then rejects the exchange, but an in-flight
// sign-in started before the PKCE upgrade is still recognized as well-formed.
func decodeOAuthFlow(key []byte, value string) (state, verifier string, ok bool) {
	if _, _, valid := decodeOAuthState(key, value); valid {
		return value, "", true
	}
	macSep := strings.LastIndexByte(value, '.')
	if macSep <= 0 || macSep == len(value)-1 {
		return "", "", false
	}
	payload, mac := value[:macSep], value[macSep+1:]
	if !hmac.Equal([]byte(mac), []byte(oauthStateMAC(key, oauthFlowMACPrefix+payload))) {
		return "", "", false
	}
	sep := strings.LastIndexByte(payload, '.')
	if sep <= 0 || sep == len(payload)-1 {
		return "", "", false
	}
	state, verifier = payload[:sep], payload[sep+1:]
	if _, _, valid := decodeOAuthState(key, state); !valid {
		return "", "", false
	}
	return state, verifier, true
}

// GoogleOAuthProvider is the subset of oauth.GoogleOAuth the auth handler
// needs. It is an interface so HTTP/integration tests can exercise the whole
// start/callback flow without calling Google.
type GoogleOAuthProvider interface {
	// AuthURL builds the consent redirect with an S256 PKCE challenge derived
	// from codeVerifier.
	AuthURL(state, codeVerifier string) string
	Exchange(ctx context.Context, code, codeVerifier string) (*oauth.GoogleUserInfo, error)
	// BuildCodeCallbackURL redirects the browser with a single-use code that
	// POST /api/v1/auth/google/exchanges redeems for the token pair.
	BuildCodeCallbackURL(code string) string
	// BuildMFACallbackURL redirects a first-factor Google sign-in to the
	// frontend with only a two-step login challenge, never tokens.
	BuildMFACallbackURL(challenge string) string
}

// SessionIssuer is the session service surface the auth handlers use.
// *service.SessionService implements it.
type SessionIssuer interface {
	Issue(ctx context.Context, userID uuid.UUID, tokenVersion int32, amr []string) (service.TokenPair, error)
	Refresh(ctx context.Context, refreshToken string, admit service.Admission) (*service.RefreshResult, error)
	Logout(ctx context.Context, refreshToken string) error
	List(ctx context.Context, userID uuid.UUID, limit int, startingAfter string) ([]service.Session, bool, error)
	Revoke(ctx context.Context, userID, sessionID uuid.UUID) error
}

// TenantLookup resolves operational tenants for the OAuth flow.
// *service.TenantService implements it.
type TenantLookup interface {
	ResolveOperational(ctx context.Context, slug string) (*service.Tenant, error)
	GetOperational(ctx context.Context, id uuid.UUID) (*service.Tenant, error)
}

// AuthDeps are the auth handler's collaborators.
type AuthDeps struct {
	Users        service.UserService
	Sessions     SessionIssuer
	Resets       service.PasswordResetService
	Verification service.EmailVerificationService
	// OAuthStateSecret derives the OAuth state MAC key (the JWT secret).
	OAuthStateSecret         string
	RequireEmailVerification bool
	GoogleOAuth              GoogleOAuthProvider
	// Cache stores the short-lived one-time Google exchange grant.
	Cache cache.Cache
	// Sealer seals that grant with the data-encryption codec.
	Sealer service.SecretSealer
	// MFA is nil only in deployments and tests that do not wire MFA; login
	// then issues tokens directly.
	MFA service.MFAService
	// Tenants validates the tenant carried through the OAuth state. Without
	// it the Google flow fails closed with 500.
	Tenants TenantLookup
}

type AuthHandler struct {
	userSvc                  service.UserService
	sessions                 SessionIssuer
	resetSvc                 service.PasswordResetService
	emailVerifSvc            service.EmailVerificationService
	requireEmailVerification bool
	googleOAuth              GoogleOAuthProvider
	tenants                  TenantLookup
	mfa                      service.MFAService
	cache                    cache.Cache
	sealer                   service.SecretSealer
	// oauthStateKey signs the OAuth state (see encodeOAuthState).
	oauthStateKey []byte
	// flags evaluates the email-verification requirement and the sign-up
	// allowance inside the Google flow. Route-level gates use Policy.Feature.
	flags features.Evaluator
	// refreshCookie is the optional BFF refresh-token transport.
	refreshCookie RefreshCookieConfig
}

// SetFeatureFlags installs the evaluator used for feature gates. It is called
// once during route registration, before the handler serves requests.
func (h *AuthHandler) SetFeatureFlags(flags features.Evaluator) {
	h.flags = flags
}

// emailVerificationRequired combines REQUIRE_EMAIL_VERIFICATION with the
// email_verification flag for the user's tenant/user targets. The flag can
// only add the requirement; it never lifts the configured one.
func (h *AuthHandler) emailVerificationRequired(ctx context.Context, member *service.Member) bool {
	if h.requireEmailVerification {
		return true
	}
	flags := h.flags
	if flags == nil {
		flags = features.NewEvaluator(features.Options{})
	}
	return flags.Enabled(ctx, identitycatalog.KeyEmailVerification,
		features.Subject{TenantID: member.Membership.TenantID, UserID: member.User.ID})
}

// NewAuthHandler wires the auth handlers.
func NewAuthHandler(deps AuthDeps) *AuthHandler {
	return &AuthHandler{
		userSvc:                  deps.Users,
		sessions:                 deps.Sessions,
		resetSvc:                 deps.Resets,
		emailVerifSvc:            deps.Verification,
		requireEmailVerification: deps.RequireEmailVerification,
		googleOAuth:              deps.GoogleOAuth,
		tenants:                  deps.Tenants,
		mfa:                      deps.MFA,
		cache:                    deps.Cache,
		sealer:                   deps.Sealer,
		oauthStateKey:            oauthStateKey(deps.OAuthStateSecret),
	}
}

type registerInput struct {
	Body dto.RegisterRequest
}

type loginInput struct {
	Body dto.LoginRequest
}

type refreshInput struct {
	Cookie       string              `header:"Cookie" doc:"__Host-refresh cookie in cookie mode."`
	SecFetchSite string              `header:"Sec-Fetch-Site" doc:"Browser fetch metadata used for the CSRF decision."`
	Origin       string              `header:"Origin" doc:"Trusted origin fallback when Sec-Fetch-Site is unavailable."`
	CSRFToken    string              `header:"X-CSRF-Token" doc:"Required with a trusted Origin when Sec-Fetch-Site is unavailable."`
	Body         *dto.RefreshRequest `required:"false" doc:"Optional when the request carries the __Host-refresh cookie."`
}

type logoutInput struct {
	Cookie       string              `header:"Cookie" doc:"__Host-refresh cookie in cookie mode."`
	SecFetchSite string              `header:"Sec-Fetch-Site" doc:"Browser fetch metadata used for the CSRF decision."`
	Origin       string              `header:"Origin" doc:"Trusted origin fallback when Sec-Fetch-Site is unavailable."`
	CSRFToken    string              `header:"X-CSRF-Token" doc:"Required with a trusted Origin when Sec-Fetch-Site is unavailable."`
	Body         *dto.RefreshRequest `required:"false" doc:"Optional when the request carries the __Host-refresh cookie."`
}

type passwordResetRequestInput struct {
	Body dto.ForgotPasswordRequest
}

type passwordResetCompleteInput struct {
	Body dto.ResetPasswordRequest
}

type emailVerificationRequestInput struct {
	Body dto.ResendVerificationRequest
}

type emailVerificationCompleteInput struct {
	Body dto.VerifyEmailRequest
}

type googleStartInput struct{}

type googleCallbackInput struct {
	Code   string `query:"code"`
	State  string `query:"state"`
	Cookie string `header:"Cookie"`
}

type googleExchangeInput struct {
	Body dto.GoogleExchangeRequest
}

type acceptInvitationInput struct {
	Body dto.AcceptInvitationRequest
}

type signupInvitationInput struct {
	Body dto.InvitationSignUpRequest
}

// Register creates a global account and its membership in the request tenant.
func (h *AuthHandler) Register(ctx context.Context, input *registerInput) (*httpapi.Created[dto.UserResponse], error) {
	member, err := h.userSvc.Register(withClientMetadata(ctx), service.RegisterInput{
		Email:    input.Body.Email,
		Password: input.Body.Password,
		Name:     input.Body.Name,
	})
	if err != nil {
		return nil, err
	}

	// The verification email is enqueued on the registration transaction by
	// the user service; there is no post-commit send to duplicate it.

	// Attribute the audit fallback to the newly registered user; WithActor
	// mutates the request info pointer the middleware will read after the
	// handler returns.
	auditActor(ctx, member.User.ID)

	return httpapi.NewCreated("/api/v1/me/profile", toUserResponse(member)), nil
}

// Login authenticates the password and returns a token pair or an MFA
// challenge.
func (h *AuthHandler) Login(ctx context.Context, input *loginInput) (*loginOutput, error) {
	ctx = withClientMetadata(ctx)
	member, err := h.userSvc.Authenticate(ctx, service.LoginInput{
		Email:    input.Body.Email,
		Password: input.Body.Password,
	})
	if err != nil {
		return nil, err
	}
	if member.Membership.TenantID != tenant.IDFromContext(ctx) {
		return nil, apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
	}
	if !member.User.EmailVerified() && h.emailVerificationRequired(ctx, member) {
		return nil, apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}

	auditActor(ctx, member.User.ID)

	if h.mfa != nil {
		required, err := h.mfa.Required(ctx, member.User.ID)
		if err != nil {
			return nil, err
		}
		if required {
			challenge, err := h.mfa.BeginChallenge(ctx, member.User.ID, service.AMRPassword)
			if err != nil {
				return nil, err
			}
			return &loginOutput{Body: dto.LoginResponse{
				User:              toUserResponse(member),
				MFARequired:       true,
				MFAChallengeToken: challenge,
			}}, nil
		}
	}

	pair, err := h.sessions.Issue(ctx, member.User.ID, member.User.TokenVersion, []string{service.AMRPassword})
	if err != nil {
		return nil, err
	}

	return &loginOutput{
		SetCookie: h.refreshCookie.cookieHeader(pair.RefreshToken),
		Body: dto.LoginResponse{
			AccessToken:  pair.AccessToken,
			RefreshToken: h.refreshCookie.jsonRefreshToken(pair.RefreshToken),
			User:         toUserResponse(member),
		},
	}, nil
}

// Refresh rotates a refresh token.
func (h *AuthHandler) Refresh(ctx context.Context, input *refreshInput) (*loginOutput, error) {
	ctx = withClientMetadata(ctx)
	token, err := h.requestRefreshToken(csrfRequest{
		Cookie:       input.Cookie,
		SecFetchSite: input.SecFetchSite,
		Origin:       input.Origin,
		CSRFToken:    input.CSRFToken,
	}, input.Body)
	if err != nil {
		return nil, err
	}
	result, err := h.sessions.Refresh(ctx, token, func(ctx context.Context, member *service.Member) error {
		if !member.User.EmailVerified() && h.emailVerificationRequired(ctx, member) {
			return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	user := toUserResponse(result.Member)
	auditActor(ctx, user.ID)

	return &loginOutput{
		SetCookie: h.refreshCookie.cookieHeader(result.Tokens.RefreshToken),
		Body: dto.LoginResponse{
			AccessToken:  result.Tokens.AccessToken,
			RefreshToken: h.refreshCookie.jsonRefreshToken(result.Tokens.RefreshToken),
			User:         user,
		},
	}, nil
}

// Logout revokes the session that owns the presented refresh token.
func (h *AuthHandler) Logout(ctx context.Context, input *logoutInput) (*logoutOutput, error) {
	token, err := h.requestRefreshToken(csrfRequest{
		Cookie:       input.Cookie,
		SecFetchSite: input.SecFetchSite,
		Origin:       input.Origin,
		CSRFToken:    input.CSRFToken,
	}, input.Body)
	if err != nil {
		return nil, err
	}
	if err := h.sessions.Logout(ctx, token); err != nil {
		return nil, err
	}
	return &logoutOutput{Status: http.StatusNoContent, SetCookie: h.refreshCookie.clearCookieHeader()}, nil
}

type logoutAllInput struct{}

// LogoutAll revokes every session of the caller in every tenant.
func (h *AuthHandler) LogoutAll(ctx context.Context, _ *logoutAllInput) (*httpapi.Empty, error) {
	if err := h.userSvc.RevokeSessions(ctx, actorID(ctx)); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// ForgotPassword enqueues a reset email without revealing whether the address
// exists.
func (h *AuthHandler) ForgotPassword(ctx context.Context, input *passwordResetRequestInput) (*httpapi.Empty, error) {
	if err := h.resetSvc.ForgotPassword(ctx, service.ForgotPasswordInput{Email: input.Body.Email}); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// ResetPassword consumes a reset token and sets the new password.
func (h *AuthHandler) ResetPassword(ctx context.Context, input *passwordResetCompleteInput) (*httpapi.Empty, error) {
	// A live reset token resolves to its account, so the password/email
	// policy can reject a password built from that address before the reset
	// stores it.
	if lookup, ok := h.resetSvc.(service.PasswordResetEmailLookup); ok {
		email, err := lookup.EmailForResetToken(ctx, input.Body.Token)
		if err != nil {
			return nil, err
		}
		if email != "" {
			if err := service.ValidatePasswordForEmail(input.Body.Password, email, "password"); err != nil {
				return nil, err
			}
		}
	}

	if err := h.resetSvc.ResetPassword(withClientMetadata(ctx), service.ResetPasswordInput{
		Token:    input.Body.Token,
		Password: input.Body.Password,
	}); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// VerifyEmail consumes a verification token.
func (h *AuthHandler) VerifyEmail(ctx context.Context, input *emailVerificationCompleteInput) (*httpapi.Empty, error) {
	if err := h.emailVerifSvc.Verify(ctx, input.Body.Token); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// ResendVerification sends a new verification link if the account exists and
// is unverified.
func (h *AuthHandler) ResendVerification(ctx context.Context, input *emailVerificationRequestInput) (*httpapi.Empty, error) {
	if err := h.emailVerifSvc.ResendVerification(ctx, input.Body.Email); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// GoogleRedirect starts the OAuth flow with PKCE and a tenant-bound signed
// state.
func (h *AuthHandler) GoogleRedirect(ctx context.Context, _ *googleStartInput) (*redirectOutput, error) {
	if h.googleOAuth == nil {
		return nil, apperror.NewNotFound("Google OAuth not configured")
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, apperror.Internal(err, "failed to generate state")
	}

	tenantID := tenant.IDFromContext(ctx)
	if tenantID == uuid.Nil {
		return nil, apperror.Internal(nil, "tenant context is required")
	}
	state := encodeOAuthState(h.oauthStateKey, hex.EncodeToString(b), tenantID)
	verifier := oauth2.GenerateVerifier()
	cookie := oauthStateCookieName + "=" + encodeOAuthFlow(h.oauthStateKey, state, verifier) +
		"; Path=/; Max-Age=300; HttpOnly; Secure; SameSite=Lax"

	return &redirectOutput{
		Location:  h.googleOAuth.AuthURL(state, verifier),
		SetCookie: cookie,
	}, nil
}

// GoogleCallback completes the OAuth flow for the tenant bound in the signed
// state and redirects with a one-time exchange code.
func (h *AuthHandler) GoogleCallback(ctx context.Context, input *googleCallbackInput) (*redirectOutput, error) {
	if h.googleOAuth == nil {
		return nil, apperror.NewNotFound("Google OAuth not configured")
	}
	tenantID, tenantSlug, verifier, err := h.verifyOAuthCallbackState(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := features.Require(ctx, h.flags, identitycatalog.KeyGoogleOAuth, features.Subject{TenantID: tenantID}); err != nil {
		return nil, err
	}
	ctx = tenant.WithIdentity(ctx, tenantID, tenantSlug)

	if input.Code == "" {
		return nil, apperror.NewBadRequest("missing authorization code")
	}
	info, err := h.googleOAuth.Exchange(ctx, input.Code, verifier)
	if err != nil {
		status := oauthErrorStatus(err)
		slog.Error("google oauth code exchange failed",
			slog.String("error_type", oauthErrorType(err)),
			slog.Int("upstream_status", status),
		)
		if errors.Is(err, oauth.ErrEmailNotVerified) {
			return nil, apperror.NewCode(identitycatalog.CodeAuthOAuthEmailNotVerified, "google account email address is not verified")
		}
		if errors.Is(err, oauth.ErrIncompleteUserInfo) {
			return nil, apperror.NewCode(identitycatalog.CodeAuthOAuthUserInfoIncomplete, "google did not return an account id and email address")
		}
		if status >= 400 && status < 500 && status != 429 {
			return nil, apperror.NewBadRequest("failed to exchange authorization code")
		}
		return nil, apperror.NewCode(identitycatalog.CodeAuthOAuthUpstreamUnavailable, "oauth provider is unavailable")
	}

	allowSignUp := features.Require(ctx, h.flags, identitycatalog.KeyUserRegistration, features.Subject{TenantID: tenantID}) == nil
	result, err := h.userSvc.SignInWithGoogle(withClientMetadata(ctx), service.GoogleSignIn{
		GoogleID:      info.ID,
		Email:         info.Email,
		Name:          info.Name,
		EmailVerified: info.VerifiedEmail,
		AllowSignUp:   allowSignUp,
		Complete: func(ctx context.Context, member *service.Member) error {
			return h.admitGoogleUser(ctx, tenantID, member)
		},
		MFARequired: func(ctx context.Context, member *service.Member) (bool, error) {
			if h.mfa == nil {
				return false, nil
			}
			return h.mfa.Required(ctx, member.User.ID)
		},
	})
	if err != nil {
		return nil, err
	}
	cleared := oauthStateCookieName + "=; Path=/; Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:00 GMT; HttpOnly; Secure; SameSite=Lax"
	if result.MFARequired {
		challenge, err := h.mfa.BeginChallenge(ctx, result.Member.User.ID, service.AMRGoogle)
		if err != nil {
			return nil, err
		}
		return &redirectOutput{Location: h.googleOAuth.BuildMFACallbackURL(challenge), SetCookie: cleared}, nil
	}
	exchangeCode, err := h.issueGoogleExchangeGrant(ctx, tenantID, result)
	if err != nil {
		return nil, err
	}
	return &redirectOutput{Location: h.googleOAuth.BuildCodeCallbackURL(exchangeCode), SetCookie: cleared}, nil
}

// googleExchangeGrant is the sealed payload behind a callback's one-time
// code. It carries the tenant that started the flow and the token response
// the exchange endpoint returns, so the code alone mints nothing.
type googleExchangeGrant struct {
	TenantID  uuid.UUID         `json:"tenantId"`
	ExpiresAt int64             `json:"exp"`
	Response  dto.LoginResponse `json:"response"`
}

func googleExchangeKey(code string) string {
	sum := sha256.Sum256([]byte(code))
	return googleExchangeKeyPrefix + hex.EncodeToString(sum[:])
}

func googleExchangeInvalid() error {
	return apperror.NewCode(identitycatalog.CodeAuthOAuthCodeInvalid, "invalid or expired google sign-in code")
}

// issueGoogleExchangeGrant stores the signed-in user's tokens under a random
// 256-bit code for googleExchangeTTL and returns the code.
func (h *AuthHandler) issueGoogleExchangeGrant(ctx context.Context, tenantID uuid.UUID, result *service.GoogleSignInResult) (string, error) {
	if h.cache == nil || h.sealer == nil {
		return "", apperror.Internal(nil, "google exchange store is not configured")
	}
	raw := make([]byte, googleExchangeCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", apperror.Internal(err, "failed to generate google exchange code")
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	grant := googleExchangeGrant{
		TenantID:  tenantID,
		ExpiresAt: time.Now().Add(googleExchangeTTL).Unix(),
		Response: dto.LoginResponse{
			AccessToken:  result.Tokens.AccessToken,
			RefreshToken: result.Tokens.RefreshToken,
			User:         toUserResponse(result.Member),
		},
	}
	payload, err := json.Marshal(grant)
	if err != nil {
		return "", apperror.Internal(err, "failed to encode google exchange grant")
	}
	sealed, err := h.sealer.Seal(payload)
	if err != nil {
		return "", apperror.Internal(err, "failed to seal google exchange grant")
	}
	if err := h.cache.Set(ctx, googleExchangeKey(code), sealed, googleExchangeTTL); err != nil {
		return "", apperror.Internal(err, "failed to store google exchange code")
	}
	return code, nil
}

// GoogleExchange redeems a Google sign-in code for the token pair.
func (h *AuthHandler) GoogleExchange(ctx context.Context, input *googleExchangeInput) (*loginOutput, error) {
	if h.cache == nil || h.sealer == nil {
		return nil, apperror.Internal(nil, "google exchange store is not configured")
	}
	key := googleExchangeKey(input.Body.Code)
	sealed, err := h.cache.Get(ctx, key)
	if err != nil {
		slog.Error("failed to read google exchange code", slog.Any("error", err))
		return nil, apperror.Internal(err, "failed to read google exchange code")
	}
	if len(sealed) == 0 {
		return nil, googleExchangeInvalid()
	}
	plain, err := h.sealer.Open(sealed)
	if err != nil {
		return nil, googleExchangeInvalid()
	}
	var grant googleExchangeGrant
	if err := json.Unmarshal(plain, &grant); err != nil {
		return nil, googleExchangeInvalid()
	}
	if grant.TenantID != tenant.IDFromContext(ctx) || !time.Now().Before(time.Unix(grant.ExpiresAt, 0)) {
		return nil, googleExchangeInvalid()
	}
	cd, ok := h.cache.(cache.CompareDeleter)
	if !ok {
		return nil, apperror.NewCode(apperror.CodeSystemUnavailable, "unable to redeem google sign-in code")
	}
	consumed, err := cd.CompareAndDelete(ctx, key, sealed)
	if err != nil {
		slog.Error("failed to consume google exchange code", slog.Any("error", err))
		return nil, apperror.Internal(err, "failed to consume google exchange code")
	}
	if !consumed {
		return nil, googleExchangeInvalid()
	}
	auditActor(ctx, grant.Response.User.ID)
	refreshToken := grant.Response.RefreshToken
	grant.Response.RefreshToken = h.refreshCookie.jsonRefreshToken(refreshToken)
	return &loginOutput{
		SetCookie: h.refreshCookie.cookieHeader(refreshToken),
		Body:      grant.Response,
	}, nil
}

// verifyOAuthCallbackState checks the CSRF state against its cookie and
// returns the active tenant bound into it plus the PKCE verifier.
func (h *AuthHandler) verifyOAuthCallbackState(ctx context.Context, input *googleCallbackInput) (tenantID uuid.UUID, tenantSlug, verifier string, err error) {
	cookieValue := cookieValue(input.Cookie, oauthStateCookieName)
	if input.State == "" || cookieValue == "" {
		return uuid.Nil, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	cookieState, verifier, ok := decodeOAuthFlow(h.oauthStateKey, cookieValue)
	if !ok || subtle.ConstantTimeCompare([]byte(input.State), []byte(cookieState)) != 1 {
		return uuid.Nil, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	_, stateTenantID, ok := decodeOAuthState(h.oauthStateKey, input.State)
	if !ok {
		return uuid.Nil, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	if h.tenants == nil {
		return uuid.Nil, "", "", apperror.Internal(nil, "oauth tenant validation is not configured")
	}
	stateTenant, err := h.tenants.GetOperational(ctx, stateTenantID)
	if err != nil {
		return uuid.Nil, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	return stateTenant.ID, stateTenant.Slug, verifier, nil
}

// oauthErrorStatus extracts the upstream HTTP status from the provider error.
func oauthErrorStatus(err error) int {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) && retrieve.Response != nil {
		return retrieve.Response.StatusCode
	}
	var upstream *oauth.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.StatusCode
	}
	return 0
}

// oauthErrorType names the recognized error class for logs without exposing
// the wrapped message.
func oauthErrorType(err error) string {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		return "oauth2.RetrieveError"
	}
	var upstream *oauth.UpstreamError
	if errors.As(err, &upstream) {
		return "oauth.UpstreamError"
	}
	if errors.Is(err, oauth.ErrEmailNotVerified) {
		return "oauth.ErrEmailNotVerified"
	}
	if errors.Is(err, oauth.ErrIncompleteUserInfo) {
		return "oauth.ErrIncompleteUserInfo"
	}
	return "transport"
}

// admitGoogleUser applies the Login/Refresh sign-in rules to the account a
// Google identity resolved to.
func (h *AuthHandler) admitGoogleUser(ctx context.Context, tenantID uuid.UUID, member *service.Member) error {
	if member.Membership.TenantID != tenantID {
		return apperror.NewUnauthorized("account is not active")
	}
	auditActor(ctx, member.User.ID)
	if !member.User.EmailVerified() && h.emailVerificationRequired(ctx, member) {
		return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}
	return nil
}
