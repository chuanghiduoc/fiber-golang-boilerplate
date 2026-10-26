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
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/oauth2"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant/tenanthttp"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/cache"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

const oauthStateCookieName = "oauth_state"

const (
	// googleExchangeTTL is how long the callback's one-time code can be
	// redeemed.
	googleExchangeTTL = 60 * time.Second
	// googleExchangeCodeBytes makes the code 256 bits of entropy.
	googleExchangeCodeBytes = 32
	googleExchangeKeyPrefix = "oauth:google:exchange:"
)

// withClientIP returns the request context carrying the client IP that the
// login lockout keys its per-source budget on. c.IP() honors Fiber's
// trusted-proxy configuration; the service clones the value.
func withClientIP(c fiber.Ctx) context.Context {
	return service.WithClientIP(c.Context(), c.IP())
}

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
// flow for tenant A and complete it under tenant B. The cookie comparison
// alone does not prove integrity, because whoever can set the state cookie
// can choose its tenant; the MAC does.
func encodeOAuthState(key []byte, nonce string, tenantID int64) string {
	payload := nonce + "." + strconv.FormatInt(tenantID, 10)
	return payload + "." + oauthStateMAC(key, payload)
}

// decodeOAuthState verifies the MAC in constant time before trusting the
// tenant carried in state.
func decodeOAuthState(key []byte, state string) (nonce string, tenantID int64, ok bool) {
	macSep := strings.LastIndexByte(state, '.')
	if macSep <= 0 || macSep == len(state)-1 {
		return "", 0, false
	}
	payload, mac := state[:macSep], state[macSep+1:]
	if !hmac.Equal([]byte(mac), []byte(oauthStateMAC(key, payload))) {
		return "", 0, false
	}
	sep := strings.LastIndexByte(payload, '.')
	if sep <= 0 || sep == len(payload)-1 {
		return "", 0, false
	}
	id, err := strconv.ParseInt(payload[sep+1:], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
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
	// POST /auth/google/exchange redeems for the token pair.
	BuildCodeCallbackURL(code string) string
	// BuildMFACallbackURL redirects a first-factor Google sign-in to the
	// frontend with only a two-step login challenge, never tokens.
	BuildMFACallbackURL(challenge string) string
}

// SessionIssuer is the session service surface the auth handlers use.
// *service.SessionService implements it.
type SessionIssuer interface {
	Issue(ctx context.Context, userID int64, tokenVersion int32, amr []string) (service.TokenPair, error)
	Refresh(ctx context.Context, refreshToken string, admit service.Admission) (*service.RefreshResult, error)
	Logout(ctx context.Context, refreshToken string) error
	List(ctx context.Context, userID int64, limit int, startingAfter string) ([]service.Session, bool, error)
	Revoke(ctx context.Context, userID, sessionID int64) error
}

// TenantLookup resolves operational tenants for the OAuth flow.
// *service.TenantService implements it.
type TenantLookup interface {
	ResolveOperational(ctx context.Context, slug string) (*service.Tenant, error)
	GetOperational(ctx context.Context, id int64) (*service.Tenant, error)
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
	// flags gates registration, Google OAuth, and the email-verification
	// requirement. The module installs app.Deps.Flags; when unset,
	// features.Require uses compiled defaults (google_oauth defaults on;
	// Google credentials remain the real switch).
	flags features.Evaluator
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

// withClient returns the request context carrying the client IP (login
// lockout, session record) and User-Agent (session record).
func withClient(c fiber.Ctx) context.Context {
	return service.WithUserAgent(withClientIP(c), c.Get(fiber.HeaderUserAgent))
}

// Register godoc
// @Summary Register a new user
// @Description Create a new user account
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.RegisterRequest true "Register request"
// @Success 201 {object} dto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails "feature.disabled when user_registration is off"
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/register [post]
func (h *AuthHandler) Register(c fiber.Ctx) error {
	if err := features.Require(c.Context(), h.flags, identitycatalog.KeyUserRegistration, features.SubjectFromContext(c.Context(), 0)); err != nil {
		return err
	}
	var req dto.RegisterRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	member, err := h.userSvc.Register(withClientIP(c), service.RegisterInput{
		Email:    req.Email,
		Password: req.Password,
		Name:     req.Name,
	})
	if err != nil {
		return err
	}

	// When the user service provisions the verification email in the
	// registration transaction, sending here would duplicate it. Fall back to
	// the post-commit send only when registration was not transactional.
	if h.emailVerifSvc != nil && !h.userSvc.RegistrationEmailsTransactional() {
		// Carry the registration tenant into the fire-and-forget send. A bare
		// background context would fall back to the default tenant and write
		// the verification token to the wrong tenant.
		tenantID := tenant.EffectiveID(c.Context())
		if member.Membership.TenantID != 0 {
			tenantID = member.Membership.TenantID
		}
		tenantSlug := tenant.SlugFromContext(c.Context())
		async.Go(func() {
			ctx := tenant.WithIdentity(context.Background(), tenantID, tenantSlug)
			if err := h.emailVerifSvc.SendVerification(ctx, member.User.ID, member.User.Email); err != nil {
				slog.Error("post-commit verification email failed", slog.Int64("user_id", member.User.ID), slog.Any("error", err))
			}
		})
	}

	// Attribute the audit event to the newly registered user. The key matches
	// middleware.JWTAuth and is read by audithttp.Middleware after the response.
	fiber.Locals(c, "user_id", member.User.ID)

	return response.Created(c, toUserResponse(member))
}

// Login godoc
// @Summary Login
// @Description Authenticate user and return access + refresh tokens
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.LoginRequest true "Login request"
// @Success 200 {object} dto.LoginResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/login [post]
func (h *AuthHandler) Login(c fiber.Ctx) error {
	var req dto.LoginRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	ctx := withClient(c)
	member, err := h.userSvc.Authenticate(ctx, service.LoginInput{
		Email:    req.Email,
		Password: req.Password,
	})
	if err != nil {
		return err
	}
	// Authenticate requires a membership in the request tenant, but keep the
	// invariant explicit: a token must never be minted outside it.
	if member.Membership.TenantID != tenant.EffectiveID(c.Context()) {
		return apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
	}
	// Authenticate already enforces REQUIRE_EMAIL_VERIFICATION; this adds the
	// email_verification flag for the user's tenant/user targets.
	if !member.User.EmailVerified() && h.emailVerificationRequired(c.Context(), member) {
		return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}

	// Attribute the audit event to the authenticated user.
	fiber.Locals(c, "user_id", member.User.ID)

	// A confirmed factor makes the password only the first step: issue a
	// short-lived challenge instead of a session.
	if h.mfa != nil {
		required, err := h.mfa.Required(c.Context(), member.User.ID)
		if err != nil {
			return err
		}
		if required {
			challenge, err := h.mfa.BeginChallenge(ctx, member.User.ID, service.AMRPassword)
			if err != nil {
				return err
			}
			return response.Success(c, dto.LoginResponse{
				User:              toUserResponse(member),
				MFARequired:       true,
				MFAChallengeToken: challenge,
			})
		}
	}

	// Issue re-checks the membership and the token version the password was
	// verified at, so a credential change committed in between fails closed.
	pair, err := h.sessions.Issue(ctx, member.User.ID, member.User.TokenVersion, []string{service.AMRPassword})
	if err != nil {
		return err
	}

	return response.Success(c, dto.LoginResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		User:         toUserResponse(member),
	})
}

// Refresh godoc
// @Summary Refresh access token
// @Description Exchange a refresh token for a new access token
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.RefreshRequest true "Refresh request"
// @Success 200 {object} dto.LoginResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/refresh [post]
func (h *AuthHandler) Refresh(c fiber.Ctx) error {
	var req dto.RefreshRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	// Refresh validates the token (tenant, reuse, expiry, session, member,
	// token version) and runs the admission rule before consuming anything,
	// so a recoverable refusal (email not yet verified) does not burn a valid
	// token. A token that was already used revokes its whole session.
	result, err := h.sessions.Refresh(withClient(c), req.RefreshToken, func(ctx context.Context, member *service.Member) error {
		if !member.User.EmailVerified() && h.emailVerificationRequired(ctx, member) {
			return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
		}
		return nil
	})
	if err != nil {
		return err
	}
	user := toUserResponse(result.Member)

	// Attribute the audit event to the authenticated user.
	fiber.Locals(c, "user_id", user.ID)

	return response.Success(c, dto.LoginResponse{
		AccessToken:  result.Tokens.AccessToken,
		RefreshToken: result.Tokens.RefreshToken,
		User:         user,
	})
}

// Logout godoc
// @Summary Logout
// @Description Revoke the session of a refresh token by possession. The tenant header is not required because possession of the opaque token authorizes revocation.
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.RefreshRequest true "Refresh token whose session is revoked"
// @Success 204
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Router /auth/logout [post]
func (h *AuthHandler) Logout(c fiber.Ctx) error {
	var req dto.RefreshRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.sessions.Logout(c.Context(), req.RefreshToken); err != nil {
		return err
	}
	return response.NoContent(c)
}

// LogoutAll godoc
// @Summary Log out everywhere
// @Description Revoke every session of the authenticated user in every tenant and invalidate every access token (token version bump)
// @Tags Auth
// @Security BearerAuth
// @Success 204
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/logout-all [post]
func (h *AuthHandler) LogoutAll(c fiber.Ctx) error {
	if err := h.userSvc.RevokeSessions(c.Context(), httpx.AuthUserID(c)); err != nil {
		return err
	}
	return response.NoContent(c)
}

// ForgotPassword godoc
// @Summary Request password reset
// @Description Send a password reset email
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.ForgotPasswordRequest true "Forgot password request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Router /auth/forgot-password [post]
func (h *AuthHandler) ForgotPassword(c fiber.Ctx) error {
	var req dto.ForgotPasswordRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.resetSvc.ForgotPassword(c.Context(), service.ForgotPasswordInput{Email: req.Email}); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "if the email exists, a reset link has been sent"})
}

// ResetPassword godoc
// @Summary Reset password
// @Description Reset password using a token. The tenant query parameter is the browser-link fallback for tenants that cannot send X-Tenant-Slug.
// @Tags Auth
// @Accept json
// @Produce json
// @Param tenant query string false "Tenant slug from the emailed reset link (X-Tenant-Slug takes precedence)"
// @Param request body dto.ResetPasswordRequest true "Reset password request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Router /auth/reset-password [post]
func (h *AuthHandler) ResetPassword(c fiber.Ctx) error {
	var req dto.ResetPasswordRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	// A live reset token resolves to its account, so the password/email
	// policy can reject a password built from that address before the reset
	// stores it. Unknown or expired tokens skip the check and fail below.
	if lookup, ok := h.resetSvc.(service.PasswordResetEmailLookup); ok {
		email, err := lookup.EmailForResetToken(c.Context(), req.Token)
		if err != nil {
			return err
		}
		if email != "" {
			if err := service.ValidatePasswordForEmail(req.Password, email, "password"); err != nil {
				return err
			}
		}
	}

	if err := h.resetSvc.ResetPassword(service.WithUserAgent(withClientIP(c), c.Get(fiber.HeaderUserAgent)), service.ResetPasswordInput{
		Token:    req.Token,
		Password: req.Password,
	}); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "password has been reset successfully"})
}

// VerifyEmail godoc
// @Summary Verify email address
// @Description Verify email using a token. The tenant query parameter is the browser-link fallback for tenants that cannot send X-Tenant-Slug.
// @Tags Auth
// @Accept json
// @Produce json
// @Param tenant query string false "Tenant slug from the emailed verification link (X-Tenant-Slug takes precedence)"
// @Param request body dto.VerifyEmailRequest true "Verify email request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Router /auth/verify-email [post]
func (h *AuthHandler) VerifyEmail(c fiber.Ctx) error {
	var req dto.VerifyEmailRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.emailVerifSvc.Verify(c.Context(), req.Token); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "email verified successfully"})
}

// ResendVerification godoc
// @Summary Resend verification email
// @Description Resend email verification link
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.ResendVerificationRequest true "Resend verification request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Router /auth/resend-verification [post]
func (h *AuthHandler) ResendVerification(c fiber.Ctx) error {
	var req dto.ResendVerificationRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.emailVerifSvc.ResendVerification(c.Context(), req.Email); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "if the email exists and is not verified, a verification link has been sent"})
}

// GoogleRedirect godoc
// @Summary Redirect to Google OAuth
// @Description Redirects to Google's OAuth consent screen. Browser navigation may select the tenant with the validated tenant query parameter; the tenant is bound into the signed state.
// @Tags Auth
// @Param tenant query string false "Tenant slug for browser navigation (X-Tenant-Slug takes precedence)"
// @Success 302
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/google [get]
func (h *AuthHandler) GoogleRedirect(c fiber.Ctx) error {
	if h.googleOAuth == nil {
		return apperror.NewNotFound("Google OAuth not configured")
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return apperror.NewInternal("failed to generate state")
	}

	// X-Tenant-Slug is normally resolved by the global tenant middleware. When
	// the handler is mounted without it (tests/minimal deployments), a
	// validated `tenant` query parameter lets browser navigation start a
	// non-default flow. Header/resolved context always wins; an explicit
	// conflicting header therefore cannot be overridden by the query.
	tenantID := tenanthttp.ID(c)
	tenantSlug := tenanthttp.Slug(c)
	if tenantID == 0 {
		slug := strings.TrimSpace(c.Query(tenant.QueryParam))
		if slug != "" {
			if !tenant.IsValidSlug(slug) {
				return apperror.NewNotFound("tenant not found")
			}
			if h.tenants == nil {
				return apperror.NewInternal("oauth tenant validation is not configured")
			}
			found, err := h.tenants.ResolveOperational(c.Context(), slug)
			if err != nil {
				if errors.Is(err, apperror.ErrNotFound) {
					return apperror.NewNotFound("tenant not found")
				}
				return apperror.NewInternal("failed to resolve tenant")
			}
			tenantID, tenantSlug = found.ID, found.Slug
		} else {
			tenantID = tenant.EffectiveID(c.Context())
			tenantSlug = tenant.SlugFromContext(c.Context())
		}
	}
	if tenantID <= 0 {
		return apperror.NewInternal("tenant context is required")
	}
	if err := features.Require(c.Context(), h.flags, identitycatalog.KeyGoogleOAuth, features.Subject{TenantID: tenantID}); err != nil {
		return err
	}
	fiber.Locals[int64](c, "tenant_id", tenantID)
	fiber.Locals[string](c, "tenant_slug", tenantSlug)
	c.SetContext(tenant.WithIdentity(c.Context(), tenantID, tenantSlug))

	state := encodeOAuthState(h.oauthStateKey, hex.EncodeToString(b), tenantID)
	verifier := oauth2.GenerateVerifier()

	c.Cookie(&fiber.Cookie{
		Name:     oauthStateCookieName,
		Value:    encodeOAuthFlow(h.oauthStateKey, state, verifier),
		HTTPOnly: true,
		Secure:   true,
		SameSite: fiber.CookieSameSiteLaxMode,
		MaxAge:   300, // 5 minutes
		Path:     "/",
	})

	return c.Redirect().To(h.googleOAuth.AuthURL(state, verifier))
}

// GoogleCallback godoc
// @Summary Google OAuth callback
// @Description Handles the callback from Google OAuth, creates/finds the user in the tenant bound to the signed state, and redirects with tokens. X-Tenant-Slug is ignored; unknown or suspended state tenants are rejected.
// @Tags Auth
// @Param code query string true "Authorization code"
// @Param state query string true "OAuth state containing the tenant bound at start"
// @Success 302
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Failure 502 {object} apperror.ProblemDetails "auth.oauth_upstream_unavailable"
// @Router /auth/google/callback [get]
func (h *AuthHandler) GoogleCallback(c fiber.Ctx) error {
	if h.googleOAuth == nil {
		return apperror.NewNotFound("Google OAuth not configured")
	}
	tenantID, tenantSlug, verifier, err := h.verifyOAuthCallbackState(c)
	if err != nil {
		return err
	}
	// Gate on the signed-state tenant: the callback skips tenant middleware,
	// so the request context tenant is not the one that started the flow.
	if err := features.Require(c.Context(), h.flags, identitycatalog.KeyGoogleOAuth, features.Subject{TenantID: tenantID}); err != nil {
		return err
	}
	fiber.Locals[int64](c, "tenant_id", tenantID)
	fiber.Locals[string](c, "tenant_slug", tenantSlug)
	c.SetContext(tenant.WithIdentity(c.Context(), tenantID, tenantSlug))
	ctx := c.Context()
	clearOAuthStateCookie(c)

	code := c.Query("code")
	if code == "" {
		return apperror.NewBadRequest("missing authorization code")
	}
	info, err := h.googleOAuth.Exchange(ctx, code, verifier)
	if err != nil {
		status := oauthErrorStatus(err)
		// Log only the error class and upstream status: the code, tokens, and
		// provider response bodies must never reach the logs.
		slog.Error("google oauth code exchange failed",
			slog.String("error_type", oauthErrorType(err)),
			slog.Int("upstream_status", status),
		)
		// A usable token whose user info cannot be trusted is a client-facing
		// refusal: the user can verify the address (or re-consent) and retry.
		// It must not masquerade as a provider outage.
		if errors.Is(err, oauth.ErrEmailNotVerified) {
			return apperror.NewCode(identitycatalog.CodeAuthOAuthEmailNotVerified, "google account email address is not verified")
		}
		if errors.Is(err, oauth.ErrIncompleteUserInfo) {
			return apperror.NewCode(identitycatalog.CodeAuthOAuthUserInfoIncomplete, "google did not return an account id and email address")
		}
		// A 4xx means Google rejected this client's code or credentials; the
		// caller must start over. A 429 is Google rate-limiting us, and
		// anything else (5xx, timeout, network) is an upstream outage: neither
		// may masquerade as a bad request.
		if status >= 400 && status < 500 && status != fiber.StatusTooManyRequests {
			return apperror.NewBadRequest("failed to exchange authorization code")
		}
		return apperror.NewCode(identitycatalog.CodeAuthOAuthUpstreamUnavailable, "oauth provider is unavailable")
	}

	// An existing or linkable account may always sign in; only creating a new
	// account is a registration, so it honors user_registration.
	allowSignUp := features.Require(ctx, h.flags, identitycatalog.KeyUserRegistration, features.Subject{TenantID: tenantID}) == nil
	result, err := h.userSvc.SignInWithGoogle(service.WithUserAgent(service.WithClientIP(ctx, c.IP()), c.Get(fiber.HeaderUserAgent)), service.GoogleSignIn{
		GoogleID:      info.ID,
		Email:         info.Email,
		Name:          info.Name,
		EmailVerified: info.VerifiedEmail,
		AllowSignUp:   allowSignUp,
		// Admission runs inside the sign-in transaction, before the session
		// is issued and auth.google_login is recorded: a refused sign-in
		// rolls back and is not audited.
		Complete: func(ctx context.Context, member *service.Member) error {
			return h.admitGoogleUser(ctx, c, tenantID, member)
		},
		MFARequired: func(ctx context.Context, member *service.Member) (bool, error) {
			if h.mfa == nil {
				return false, nil
			}
			return h.mfa.Required(ctx, member.User.ID)
		},
	})
	if err != nil {
		return err
	}
	if result.MFARequired {
		challenge, err := h.mfa.BeginChallenge(ctx, result.Member.User.ID, service.AMRGoogle)
		if err != nil {
			return err
		}
		return c.Redirect().To(h.googleOAuth.BuildMFACallbackURL(challenge))
	}
	exchangeCode, err := h.issueGoogleExchangeGrant(ctx, tenantID, result)
	if err != nil {
		return err
	}
	return c.Redirect().To(h.googleOAuth.BuildCodeCallbackURL(exchangeCode))
}

// googleExchangeGrant is the sealed payload behind a callback's one-time
// code. It carries the tenant that started the flow and the token response
// the exchange endpoint returns, so the code alone mints nothing.
type googleExchangeGrant struct {
	TenantID  int64             `json:"tenantId"`
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
// 256-bit code for googleExchangeTTL and returns the code. The tokens never
// appear in the redirect URL.
func (h *AuthHandler) issueGoogleExchangeGrant(ctx context.Context, tenantID int64, result *service.GoogleSignInResult) (string, error) {
	if h.cache == nil || h.sealer == nil {
		return "", apperror.NewInternal("google exchange store is not configured")
	}
	raw := make([]byte, googleExchangeCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", apperror.NewInternal("failed to generate google exchange code")
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
		return "", apperror.NewInternal("failed to encode google exchange grant")
	}
	sealed, err := h.sealer.Seal(payload)
	if err != nil {
		return "", apperror.NewInternal("failed to seal google exchange grant")
	}
	if err := h.cache.Set(ctx, googleExchangeKey(code), sealed, googleExchangeTTL); err != nil {
		return "", apperror.NewInternal("failed to store google exchange code")
	}
	return code, nil
}

// GoogleExchange godoc
// @Summary Redeem a Google sign-in code
// @Description Exchange the single-use code from the Google callback's URL fragment for the normal access and refresh token pair. The code expires after 60 seconds, works once, and is bound to the tenant that started the flow.
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.GoogleExchangeRequest true "One-time code from the callback fragment"
// @Success 200 {object} dto.LoginResponse
// @Failure 401 {object} apperror.ProblemDetails "auth.oauth_code_invalid"
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/google/exchange [post]
func (h *AuthHandler) GoogleExchange(c fiber.Ctx) error {
	var req dto.GoogleExchangeRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	if h.cache == nil || h.sealer == nil {
		return apperror.NewInternal("google exchange store is not configured")
	}
	key := googleExchangeKey(req.Code)
	sealed, err := h.cache.Get(c.Context(), key)
	if err != nil {
		slog.Error("failed to read google exchange code", slog.Any("error", err))
		return apperror.NewInternal("failed to read google exchange code")
	}
	if len(sealed) == 0 {
		return googleExchangeInvalid()
	}
	plain, err := h.sealer.Open(sealed)
	if err != nil {
		return googleExchangeInvalid()
	}
	var grant googleExchangeGrant
	if err := json.Unmarshal(plain, &grant); err != nil {
		return googleExchangeInvalid()
	}
	if grant.TenantID != tenant.EffectiveID(c.Context()) || !time.Now().Before(time.Unix(grant.ExpiresAt, 0)) {
		return googleExchangeInvalid()
	}
	// Read-then-compare-and-delete keeps redemption single-use: when two
	// callers hold the same code, exactly one CompareAndDelete succeeds.
	cd, ok := h.cache.(cache.CompareDeleter)
	if !ok {
		return apperror.NewCode(apperror.CodeSystemUnavailable, "unable to redeem google sign-in code")
	}
	consumed, err := cd.CompareAndDelete(c.Context(), key, sealed)
	if err != nil {
		slog.Error("failed to consume google exchange code", slog.Any("error", err))
		return apperror.NewInternal("failed to consume google exchange code")
	}
	if !consumed {
		return googleExchangeInvalid()
	}
	fiber.Locals(c, "user_id", grant.Response.User.ID)
	return response.Success(c, grant.Response)
}

// verifyOAuthCallbackState checks the CSRF state against its cookie and
// returns the active tenant bound into it plus the PKCE verifier stored in the
// same signed cookie. The callback ignores X-Tenant-Slug: the signed state is
// the only trusted tenant source for this browser redirect.
func (h *AuthHandler) verifyOAuthCallbackState(c fiber.Ctx) (tenantID int64, tenantSlug, verifier string, err error) {
	state := c.Query("state")
	cookieValue := c.Cookies(oauthStateCookieName)
	if state == "" || cookieValue == "" {
		return 0, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	cookieState, verifier, ok := decodeOAuthFlow(h.oauthStateKey, cookieValue)
	if !ok || subtle.ConstantTimeCompare([]byte(state), []byte(cookieState)) != 1 {
		return 0, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	_, stateTenantID, ok := decodeOAuthState(h.oauthStateKey, state)
	if !ok {
		return 0, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	if h.tenants == nil {
		return 0, "", "", apperror.NewInternal("oauth tenant validation is not configured")
	}
	// GetOperational only returns operational tenants, so a tenant suspended during the
	// consent round-trip cannot complete the login.
	stateTenant, err := h.tenants.GetOperational(c.Context(), stateTenantID)
	if err != nil {
		return 0, "", "", apperror.NewBadRequest("invalid oauth state")
	}
	return stateTenant.ID, stateTenant.Slug, verifier, nil
}

// oauthErrorStatus extracts the upstream HTTP status from the provider error,
// or 0 when the failure never reached an HTTP response (timeout, DNS, TLS).
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
// the wrapped message (which may contain the code or a response body).
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

func clearOAuthStateCookie(c fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     oauthStateCookieName,
		Value:    "",
		HTTPOnly: true,
		Secure:   true,
		SameSite: fiber.CookieSameSiteLaxMode,
		MaxAge:   -1,
		Path:     "/",
		Expires:  time.Now().Add(-1 * time.Hour),
	})
}

// admitGoogleUser applies the Login/Refresh sign-in rules to the account a
// Google identity resolved to. A Google-linked account whose email was later
// changed (which clears email_verified_at) must not mint a session.
func (h *AuthHandler) admitGoogleUser(ctx context.Context, c fiber.Ctx, tenantID int64, member *service.Member) error {
	if member.Membership.TenantID != tenantID {
		return apperror.NewUnauthorized("account is not active")
	}
	// Attribute request-level audit/logging to the signed-in user, as Login does.
	fiber.Locals(c, "user_id", member.User.ID)
	if !member.User.EmailVerified() && h.emailVerificationRequired(ctx, member) {
		return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}
	return nil
}
