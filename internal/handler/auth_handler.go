package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/oauth"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

const oauthStateCookieName = "oauth_state"

type AuthHandler struct {
	userSvc       service.UserService
	refreshSvc    service.RefreshTokenService
	resetSvc      service.PasswordResetService
	emailVerifSvc service.EmailVerificationService
	jwtSecret     string
	jwtExpireHour int
	googleOAuth   *oauth.GoogleOAuth
}

func NewAuthHandler(
	userSvc service.UserService,
	refreshSvc service.RefreshTokenService,
	resetSvc service.PasswordResetService,
	emailVerifSvc service.EmailVerificationService,
	jwtSecret string,
	jwtExpireHour int,
	googleOAuth *oauth.GoogleOAuth,
) *AuthHandler {
	return &AuthHandler{
		userSvc:       userSvc,
		refreshSvc:    refreshSvc,
		resetSvc:      resetSvc,
		emailVerifSvc: emailVerifSvc,
		jwtSecret:     jwtSecret,
		jwtExpireHour: jwtExpireHour,
		googleOAuth:   googleOAuth,
	}
}

// Register godoc
// @Summary Register a new user
// @Description Create a new user account
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.RegisterRequest true "Register request"
// @Success 201 {object} response.Response{data=dto.UserResponse}
// @Failure 400 {object} response.Response
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /auth/register [post]
func (h *AuthHandler) Register(c fiber.Ctx) error {
	var req dto.RegisterRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.userSvc.Register(c.Context(), req)
	if err != nil {
		return err
	}

	// Fire-and-forget email verification
	if h.emailVerifSvc != nil {
		async.Go(func() {
			_ = h.emailVerifSvc.SendVerification(context.Background(), user.ID, user.Email)
		})
	}

	return response.Created(c, user)
}

// Login godoc
// @Summary Login
// @Description Authenticate user and return access + refresh tokens
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.LoginRequest true "Login request"
// @Success 200 {object} response.Response{data=dto.LoginResponse}
// @Failure 401 {object} response.Response
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /auth/login [post]
func (h *AuthHandler) Login(c fiber.Ctx) error {
	var req dto.LoginRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.userSvc.Authenticate(c.Context(), req)
	if err != nil {
		return err
	}

	accessToken, err := token.Generate(user.ID, user.Email, user.Role, h.jwtSecret, h.jwtExpireHour)
	if err != nil {
		return apperror.NewInternal("failed to generate access token")
	}

	refreshToken, err := h.refreshSvc.Create(c.Context(), user.ID)
	if err != nil {
		return err
	}

	return response.Success(c, dto.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		User:         *service.ToUserResponse(user),
	})
}

// Refresh godoc
// @Summary Refresh access token
// @Description Exchange a refresh token for a new access token
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.RefreshRequest true "Refresh request"
// @Success 200 {object} response.Response{data=dto.LoginResponse}
// @Failure 401 {object} response.Response
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /auth/refresh [post]
func (h *AuthHandler) Refresh(c fiber.Ctx) error {
	var req dto.RefreshRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	rt, err := h.refreshSvc.Verify(c.Context(), req.RefreshToken)
	if err != nil {
		return err
	}

	// Revoke old refresh token — if this fails, do NOT issue new tokens to prevent token reuse attacks
	if err := h.refreshSvc.Revoke(c.Context(), req.RefreshToken); err != nil {
		return apperror.NewInternal("failed to revoke refresh token")
	}

	user, err := h.userSvc.GetByID(c.Context(), rt.UserID)
	if err != nil {
		return err
	}

	accessToken, err := token.Generate(user.ID, user.Email, user.Role, h.jwtSecret, h.jwtExpireHour)
	if err != nil {
		return apperror.NewInternal("failed to generate access token")
	}

	newRefreshToken, err := h.refreshSvc.Create(c.Context(), rt.UserID)
	if err != nil {
		return err
	}

	return response.Success(c, dto.LoginResponse{
		AccessToken:  accessToken,
		RefreshToken: newRefreshToken,
		User:         *user,
	})
}

// Logout godoc
// @Summary Logout
// @Description Revoke a refresh token
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.RefreshRequest true "Refresh token to revoke"
// @Success 204
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Router /auth/logout [post]
func (h *AuthHandler) Logout(c fiber.Ctx) error {
	var req dto.RefreshRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	_ = h.refreshSvc.Revoke(c.Context(), req.RefreshToken)
	return response.NoContent(c)
}

// ForgotPassword godoc
// @Summary Request password reset
// @Description Send a password reset email
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.ForgotPasswordRequest true "Forgot password request"
// @Success 200 {object} response.Response
// @Failure 400 {object} response.Response
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Router /auth/forgot-password [post]
func (h *AuthHandler) ForgotPassword(c fiber.Ctx) error {
	var req dto.ForgotPasswordRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.resetSvc.ForgotPassword(c.Context(), req); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "if the email exists, a reset link has been sent"})
}

// ResetPassword godoc
// @Summary Reset password
// @Description Reset password using a token
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.ResetPasswordRequest true "Reset password request"
// @Success 200 {object} response.Response
// @Failure 400 {object} response.Response
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Router /auth/reset-password [post]
func (h *AuthHandler) ResetPassword(c fiber.Ctx) error {
	var req dto.ResetPasswordRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.resetSvc.ResetPassword(c.Context(), req); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "password has been reset successfully"})
}

// VerifyEmail godoc
// @Summary Verify email address
// @Description Verify email using a token
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.VerifyEmailRequest true "Verify email request"
// @Success 200 {object} response.Response
// @Failure 400 {object} response.Response
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Router /auth/verify-email [post]
func (h *AuthHandler) VerifyEmail(c fiber.Ctx) error {
	var req dto.VerifyEmailRequest
	if err := bindAndValidate(c, &req); err != nil {
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
// @Success 200 {object} response.Response
// @Failure 400 {object} response.Response
// @Failure 422 {object} response.Response
// @Failure 429 {object} response.Response
// @Router /auth/resend-verification [post]
func (h *AuthHandler) ResendVerification(c fiber.Ctx) error {
	var req dto.ResendVerificationRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.emailVerifSvc.ResendVerification(c.Context(), req.Email); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "if the email exists and is not verified, a verification link has been sent"})
}

// GoogleRedirect godoc
// @Summary Redirect to Google OAuth
// @Description Redirects the user to Google's OAuth consent screen
// @Tags Auth
// @Success 302
// @Failure 404 {object} response.Response
// @Failure 429 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /auth/google [get]
func (h *AuthHandler) GoogleRedirect(c fiber.Ctx) error {
	if h.googleOAuth == nil {
		return apperror.NewNotFound("Google OAuth not configured")
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return apperror.NewInternal("failed to generate state")
	}
	state := hex.EncodeToString(b)

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
// @Description Handles the callback from Google OAuth, creates/finds user and redirects with tokens
// @Tags Auth
// @Param code query string true "Authorization code"
// @Success 302
// @Failure 400 {object} response.Response
// @Failure 404 {object} response.Response
// @Failure 429 {object} response.Response
// @Failure 500 {object} response.Response
// @Router /auth/google/callback [get]
func (h *AuthHandler) GoogleCallback(c fiber.Ctx) error {
	if h.googleOAuth == nil {
		return apperror.NewNotFound("Google OAuth not configured")
	}

	// Verify CSRF state
	state := c.Query("state")
	cookieState := c.Cookies(oauthStateCookieName)
	if state == "" || cookieState == "" || state != cookieState {
		return apperror.NewBadRequest("invalid oauth state")
	}

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

	info, err := h.googleOAuth.Exchange(c.Context(), code)
	if err != nil {
		return apperror.NewBadRequest("failed to exchange authorization code")
	}

	user, err := h.userSvc.FindOrCreateByGoogle(c.Context(), info.ID, info.Email, info.Name)
	if err != nil {
		return err
	}

	accessToken, err := token.Generate(user.ID, user.Email, user.Role, h.jwtSecret, h.jwtExpireHour)
	if err != nil {
		return apperror.NewInternal("failed to generate token")
	}

	refreshToken, err := h.refreshSvc.Create(c.Context(), user.ID)
	if err != nil {
		return apperror.NewInternal("failed to generate refresh token")
	}

	redirectURL := h.googleOAuth.BuildCallbackURL(accessToken, refreshToken)
	return c.Redirect().To(redirectURL)
}
