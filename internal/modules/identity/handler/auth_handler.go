package handler

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/hkdf"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const oauthStateCookieName = "oauth_state"

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

// GoogleOAuthProvider is the subset of oauth.GoogleOAuth the auth handler
// needs. It is an interface so HTTP/integration tests can exercise the whole
// start/callback flow without calling Google.
type GoogleOAuthProvider interface {
	AuthURL(state string) string
	Exchange(ctx context.Context, code string) (*oauth.GoogleUserInfo, error)
	BuildCallbackURL(accessToken, refreshToken string) string
}

type AuthHandler struct {
	userSvc                  service.UserService
	refreshSvc               service.RefreshTokenService
	resetSvc                 service.PasswordResetService
	emailVerifSvc            service.EmailVerificationService
	jwtSecret                string
	jwtExpireHour            int
	requireEmailVerification bool
	googleOAuth              GoogleOAuthProvider
	// tenants is optional for source compatibility with handler tests and
	// minimal deployments. Production wiring supplies it so the Google
	// redirect/callback can resolve and validate the tenant carried through
	// the OAuth state.
	tenants repository.TenantRepository
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
func (h *AuthHandler) emailVerificationRequired(ctx context.Context, user *dto.UserResponse) bool {
	if h.requireEmailVerification {
		return true
	}
	flags := h.flags
	if flags == nil {
		flags = features.NewEvaluator(features.Options{})
	}
	return flags.Enabled(ctx, identitycatalog.KeyEmailVerification,
		features.Subject{TenantID: user.TenantID, UserID: user.ID})
}

func NewAuthHandler(
	userSvc service.UserService,
	refreshSvc service.RefreshTokenService,
	resetSvc service.PasswordResetService,
	emailVerifSvc service.EmailVerificationService,
	jwtSecret string,
	jwtExpireHour int,
	requireEmailVerification bool,
	googleOAuth GoogleOAuthProvider,
	tenants ...repository.TenantRepository,
) *AuthHandler {
	var tenantRepo repository.TenantRepository
	if len(tenants) > 0 {
		tenantRepo = tenants[0]
	}
	return &AuthHandler{
		userSvc:                  userSvc,
		refreshSvc:               refreshSvc,
		resetSvc:                 resetSvc,
		emailVerifSvc:            emailVerifSvc,
		jwtSecret:                jwtSecret,
		jwtExpireHour:            jwtExpireHour,
		requireEmailVerification: requireEmailVerification,
		googleOAuth:              googleOAuth,
		tenants:                  tenantRepo,
		oauthStateKey:            oauthStateKey(jwtSecret),
	}
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

	user, err := h.userSvc.Register(withClientIP(c), req)
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
		if user.TenantID != 0 {
			tenantID = user.TenantID
		}
		tenantSlug := tenant.SlugFromContext(c.Context())
		async.Go(func() {
			ctx := tenant.WithIdentity(context.Background(), tenantID, tenantSlug)
			_ = h.emailVerifSvc.SendVerification(ctx, user.ID, user.Email)
		})
	}

	// Attribute the audit event to the newly registered user. The key matches
	// middleware.JWTAuth and is read by audit.Middleware after the response.
	fiber.Locals(c, "user_id", user.ID)

	return response.Created(c, user)
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

	user, err := h.userSvc.Authenticate(withClientIP(c), req)
	if err != nil {
		return err
	}
	// Authenticate is tenant-scoped, but keep the invariant explicit: a token
	// must never be minted for a user outside the request tenant.
	if user.TenantID != tenant.EffectiveID(c.Context()) {
		return apperror.NewCode(identitycatalog.CodeAuthInvalidCredentials, "invalid email or password")
	}
	// Authenticate already enforces REQUIRE_EMAIL_VERIFICATION; this adds the
	// email_verification flag for the user's tenant/user targets.
	if !user.EmailVerified && h.emailVerificationRequired(c.Context(), user) {
		return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}

	accessToken, err := token.GenerateWithVersionAndTenant(user.ID, user.TenantID, user.Email, user.Role, user.TokenVersion, h.jwtSecret, h.jwtExpireHour)
	if err != nil {
		return apperror.NewInternal("failed to generate access token")
	}

	refreshToken, err := h.refreshSvc.Create(c.Context(), user.ID, user.TokenVersion)
	if err != nil {
		return err
	}

	// Attribute the audit event to the authenticated user.
	fiber.Locals(c, "user_id", user.ID)

	return response.Success(c, dto.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		User:         *user,
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

	// Look the token up without consuming it first, so a recoverable refusal
	// (email not yet verified) does not burn a valid token. Verify checks
	// expiry and the request tenant.
	ownerID, err := h.refreshSvc.Verify(c.Context(), req.RefreshToken)
	if err != nil {
		return err
	}
	user, err := h.userSvc.GetByID(c.Context(), ownerID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewUnauthorized("account is not active")
		}
		return err
	}
	if !user.EmailVerified && h.emailVerificationRequired(c.Context(), user) {
		return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}

	// Consume atomically: a replayed or concurrent refresh token can only be
	// used once, even if two requests arrive at the same time.
	userID, tokenTenantID, tokenVersion, err := h.refreshSvc.Consume(c.Context(), req.RefreshToken)
	if err != nil {
		return err
	}
	if userID != user.ID {
		return apperror.NewUnauthorized("refresh token is no longer valid")
	}
	// Refresh tokens are scoped to the request tenant. A token issued by
	// tenant A can never be replayed against tenant B (or the default tenant
	// when the caller omitted the header).
	if tokenTenantID != tenant.EffectiveID(c.Context()) {
		return apperror.NewUnauthorized("refresh token is no longer valid")
	}
	if user.TenantID != tokenTenantID {
		return apperror.NewUnauthorized("refresh token is no longer valid")
	}
	// A token issued before a password change/reset, ban, or role change must
	// not be allowed to mint a new session. CreateRefreshToken re-checks the
	// stored version, so a change committed after the lookup above still
	// cannot mint a refresh token.
	if user.TokenVersion != tokenVersion {
		return apperror.NewUnauthorized("refresh token is no longer valid")
	}

	accessToken, err := token.GenerateWithVersionAndTenant(user.ID, user.TenantID, user.Email, user.Role, user.TokenVersion, h.jwtSecret, h.jwtExpireHour)
	if err != nil {
		return apperror.NewInternal("failed to generate access token")
	}

	newRefreshToken, err := h.refreshSvc.Create(c.Context(), userID, tokenVersion)
	if err != nil {
		return err
	}

	// Attribute the audit event to the authenticated user.
	fiber.Locals(c, "user_id", user.ID)

	return response.Success(c, dto.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
		User:         *user,
	})
}

// Logout godoc
// @Summary Logout
// @Description Revoke one refresh token by possession. The tenant header is not required because possession of the opaque token authorizes revocation.
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.RefreshRequest true "Refresh token to revoke"
// @Success 204
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Router /auth/logout [post]
func (h *AuthHandler) Logout(c fiber.Ctx) error {
	var req dto.RefreshRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.refreshSvc.Revoke(c.Context(), req.RefreshToken); err != nil {
		return apperror.NewInternal("failed to revoke refresh token")
	}
	return response.NoContent(c)
}

// LogoutAll godoc
// @Summary Revoke all refresh tokens
// @Description Revoke every refresh token for the authenticated user
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

	if err := h.resetSvc.ForgotPassword(c.Context(), req); err != nil {
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

	if err := h.resetSvc.ResetPassword(withClientIP(c), req); err != nil {
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
	tenantID := tenant.ID(c)
	tenantSlug := tenant.Slug(c)
	if tenantID == 0 {
		slug := strings.TrimSpace(c.Query(tenant.QueryParam))
		if slug != "" {
			if !tenant.IsValidSlug(slug) {
				return apperror.NewNotFound("tenant not found")
			}
			if h.tenants == nil {
				return apperror.NewInternal("oauth tenant validation is not configured")
			}
			found, err := h.tenants.ResolveBySlug(c.Context(), slug)
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

	c.Cookie(&fiber.Cookie{
		Name:     oauthStateCookieName,
		Value:    state,
		HTTPOnly: true,
		Secure:   true,
		SameSite: fiber.CookieSameSiteLaxMode,
		MaxAge:   300, // 5 minutes
		Path:     "/",
	})

	return c.Redirect().To(h.googleOAuth.AuthURL(state))
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
// @Router /auth/google/callback [get]
func (h *AuthHandler) GoogleCallback(c fiber.Ctx) error {
	if h.googleOAuth == nil {
		return apperror.NewNotFound("Google OAuth not configured")
	}
	tenantID, tenantSlug, err := h.verifyOAuthCallbackState(c)
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
	info, err := h.googleOAuth.Exchange(ctx, code)
	if err != nil {
		return apperror.NewBadRequest("failed to exchange authorization code")
	}

	// An existing or linkable account may always sign in; only creating a new
	// account is a registration, so it honors user_registration.
	allowSignUp := features.Require(ctx, h.flags, identitycatalog.KeyUserRegistration, features.Subject{TenantID: tenantID}) == nil
	var accessToken, refreshToken string
	_, err = h.userSvc.SignInWithGoogle(ctx, service.GoogleSignIn{
		GoogleID:    info.ID,
		Email:       info.Email,
		Name:        info.Name,
		AllowSignUp: allowSignUp,
		// Admission and session issuance run inside the sign-in transaction,
		// before auth.google_login is recorded: a refused sign-in, or one
		// whose session cannot be stored, rolls back and is not audited.
		Complete: func(ctx context.Context, tx pgx.Tx, user *dto.UserResponse) error {
			if err := h.admitGoogleUser(ctx, c, tenantID, user); err != nil {
				return err
			}
			var issueErr error
			accessToken, refreshToken, issueErr = h.issueGoogleSession(ctx, tx, user)
			return issueErr
		},
	})
	if err != nil {
		return err
	}
	return c.Redirect().To(h.googleOAuth.BuildCallbackURL(accessToken, refreshToken))
}

// verifyOAuthCallbackState checks the CSRF state against its cookie and
// returns the active tenant bound into it. The callback ignores
// X-Tenant-Slug: the signed state is the only trusted tenant source for this
// browser redirect.
func (h *AuthHandler) verifyOAuthCallbackState(c fiber.Ctx) (tenantID int64, tenantSlug string, err error) {
	state := c.Query("state")
	cookieState := c.Cookies(oauthStateCookieName)
	if state == "" || cookieState == "" ||
		subtle.ConstantTimeCompare([]byte(state), []byte(cookieState)) != 1 {
		return 0, "", apperror.NewBadRequest("invalid oauth state")
	}
	_, stateTenantID, ok := decodeOAuthState(h.oauthStateKey, state)
	if !ok {
		return 0, "", apperror.NewBadRequest("invalid oauth state")
	}
	if h.tenants == nil {
		return 0, "", apperror.NewInternal("oauth tenant validation is not configured")
	}
	// GetByID only returns active tenants, so a tenant suspended during the
	// consent round-trip cannot complete the login.
	stateTenant, err := h.tenants.GetByID(c.Context(), stateTenantID)
	if err != nil {
		return 0, "", apperror.NewBadRequest("invalid oauth state")
	}
	return stateTenant.ID, stateTenant.Slug, nil
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
func (h *AuthHandler) admitGoogleUser(ctx context.Context, c fiber.Ctx, tenantID int64, user *dto.UserResponse) error {
	if user.TenantID != tenantID {
		return apperror.NewUnauthorized("account is not active")
	}
	// Attribute request-level audit/logging to the signed-in user, as Login does.
	fiber.Locals(c, "user_id", user.ID)
	if !user.EmailVerified && h.emailVerificationRequired(ctx, user) {
		return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}
	return nil
}

// issueGoogleSession mints the access token and stores the refresh token in
// the sign-in transaction.
func (h *AuthHandler) issueGoogleSession(ctx context.Context, tx pgx.Tx, user *dto.UserResponse) (accessToken, refreshToken string, err error) {
	accessToken, err = token.GenerateWithVersionAndTenant(user.ID, user.TenantID, user.Email, user.Role, user.TokenVersion, h.jwtSecret, h.jwtExpireHour)
	if err != nil {
		return "", "", apperror.NewInternal("failed to generate token")
	}
	refreshToken, err = h.refreshSvc.CreateTx(ctx, tx, user.ID, user.TokenVersion)
	if err != nil {
		return "", "", apperror.NewInternal("failed to generate refresh token")
	}
	return accessToken, refreshToken, nil
}
