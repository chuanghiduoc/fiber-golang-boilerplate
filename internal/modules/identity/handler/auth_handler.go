package handler

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const oauthStateCookieName = "oauth_state"

// encodeOAuthState binds the OAuth state nonce to the tenant that started the
// flow. The callback must use this tenant instead of X-Tenant-Slug, otherwise
// an attacker could start a flow for tenant A and complete it under tenant B.
func encodeOAuthState(nonce string, tenantID int64) string {
	return nonce + "." + strconv.FormatInt(tenantID, 10)
}

func decodeOAuthState(state string) (nonce string, tenantID int64, ok bool) {
	sep := strings.LastIndexByte(state, '.')
	if sep <= 0 || sep == len(state)-1 {
		return "", 0, false
	}
	id, err := strconv.ParseInt(state[sep+1:], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	return state[:sep], id, true
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
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/register [post]
func (h *AuthHandler) Register(c fiber.Ctx) error {
	var req dto.RegisterRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.userSvc.Register(c.Context(), req)
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

	user, err := h.userSvc.Authenticate(c.Context(), req)
	if err != nil {
		return err
	}
	// Authenticate is tenant-scoped, but keep the invariant explicit: a token
	// must never be minted for a user outside the request tenant.
	if user.TenantID != tenant.EffectiveID(c.Context()) {
		return apperror.NewCode(apperror.CodeAuthInvalidCredentials, "invalid email or password")
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

	// Consume atomically: a replayed or concurrent refresh token can only be
	// used once, even if two requests arrive at the same time.
	userID, tokenTenantID, tokenVersion, err := h.refreshSvc.Consume(c.Context(), req.RefreshToken)
	if err != nil {
		return err
	}
	// Refresh tokens are scoped to the request tenant. A token issued by
	// tenant A can never be replayed against tenant B (or the default tenant
	// when the caller omitted the header).
	if tokenTenantID != tenant.EffectiveID(c.Context()) {
		return apperror.NewUnauthorized("refresh token is no longer valid")
	}

	user, err := h.userSvc.GetByID(c.Context(), userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return apperror.NewUnauthorized("account is not active")
		}
		return err
	}
	if user.TenantID != tokenTenantID {
		return apperror.NewUnauthorized("refresh token is no longer valid")
	}
	// A token issued before a password change/reset, ban, or role change must
	// not be allowed to mint a new session.
	if user.TokenVersion != tokenVersion {
		return apperror.NewUnauthorized("refresh token is no longer valid")
	}
	if h.requireEmailVerification && !user.EmailVerified {
		return apperror.NewCode(apperror.CodeAuthEmailNotVerified, "email not verified")
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

	if err := h.resetSvc.ResetPassword(c.Context(), req); err != nil {
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
	fiber.Locals[int64](c, "tenant_id", tenantID)
	fiber.Locals[string](c, "tenant_slug", tenantSlug)
	c.SetContext(tenant.WithIdentity(c.Context(), tenantID, tenantSlug))

	state := encodeOAuthState(hex.EncodeToString(b), tenantID)

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

	// Verify CSRF state. The state also carries the tenant that started the
	// flow; the callback ignores X-Tenant-Slug and scopes every lookup to it.
	state := c.Query("state")
	cookieState := c.Cookies(oauthStateCookieName)
	if state == "" || cookieState == "" ||
		subtle.ConstantTimeCompare([]byte(state), []byte(cookieState)) != 1 {
		return apperror.NewBadRequest("invalid oauth state")
	}
	_, tenantID, ok := decodeOAuthState(state)
	if !ok {
		return apperror.NewBadRequest("invalid oauth state")
	}
	if h.tenants == nil {
		return apperror.NewInternal("oauth tenant validation is not configured")
	}
	// GetByID only returns active tenants, so a tenant suspended during the
	// consent round-trip cannot complete the login. Ignore X-Tenant-Slug here:
	// the signed state is the only trusted source for this browser redirect.
	stateTenant, err := h.tenants.GetByID(c.Context(), tenantID)
	if err != nil {
		return apperror.NewBadRequest("invalid oauth state")
	}
	fiber.Locals[int64](c, "tenant_id", stateTenant.ID)
	fiber.Locals[string](c, "tenant_slug", stateTenant.Slug)
	c.SetContext(tenant.WithIdentity(c.Context(), stateTenant.ID, stateTenant.Slug))
	ctx := c.Context()

	// Clear state cookie
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

	code := c.Query("code")
	if code == "" {
		return apperror.NewBadRequest("missing authorization code")
	}

	info, err := h.googleOAuth.Exchange(ctx, code)
	if err != nil {
		return apperror.NewBadRequest("failed to exchange authorization code")
	}

	user, err := h.userSvc.FindOrCreateByGoogle(ctx, info.ID, info.Email, info.Name)
	if err != nil {
		return err
	}
	if user.TenantID != tenantID {
		return apperror.NewUnauthorized("account is not active")
	}

	accessToken, err := token.GenerateWithVersionAndTenant(user.ID, user.TenantID, user.Email, user.Role, user.TokenVersion, h.jwtSecret, h.jwtExpireHour)
	if err != nil {
		return apperror.NewInternal("failed to generate token")
	}

	refreshToken, err := h.refreshSvc.Create(ctx, user.ID, user.TokenVersion)
	if err != nil {
		return apperror.NewInternal("failed to generate refresh token")
	}

	redirectURL := h.googleOAuth.BuildCallbackURL(accessToken, refreshToken)
	return c.Redirect().To(redirectURL)
}
