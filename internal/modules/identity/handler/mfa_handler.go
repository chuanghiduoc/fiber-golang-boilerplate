package handler

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// MFAHandler serves the authenticated MFA management endpoints and the public
// two-step login verification.
type MFAHandler struct {
	mfa service.MFAService
}

func NewMFAHandler(mfa service.MFAService) *MFAHandler {
	return &MFAHandler{mfa: mfa}
}

// Enroll godoc
// @Summary Start TOTP enrollment
// @Description Generate a TOTP secret and return its otpauth provisioning URI. The secret is never returned again; re-enrolling before confirmation replaces it.
// @Tags Auth
// @Security BearerAuth
// @Produce json
// @Success 200 {object} dto.MFAEnrollResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "auth.mfa_already_enabled"
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/mfa/totp/enroll [post]
func (h *MFAHandler) Enroll(c fiber.Ctx) error {
	enrollment, err := h.mfa.EnrollTOTP(c.Context(), httpx.AuthUserID(c))
	if err != nil {
		return err
	}
	return response.Success(c, dto.MFAEnrollResponse{Type: service.MFAFactorTOTP, OtpauthURI: enrollment.OtpauthURI})
}

// Confirm godoc
// @Summary Confirm TOTP enrollment
// @Description Verify the first TOTP code, enable the factor, and return ten single-use recovery codes. This is the only time the recovery codes are shown.
// @Tags Auth
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body dto.MFAConfirmRequest true "TOTP code"
// @Success 200 {object} dto.MFARecoveryCodesResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/mfa/totp/confirm [post]
func (h *MFAHandler) Confirm(c fiber.Ctx) error {
	var req dto.MFAConfirmRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	codes, err := h.mfa.ConfirmTOTP(c.Context(), httpx.AuthUserID(c), fiber.Locals[int64](c, middleware.LocalSessionID), req.Code)
	if err != nil {
		return err
	}
	return response.Success(c, dto.MFARecoveryCodesResponse{RecoveryCodes: codes})
}

// Disable godoc
// @Summary Disable MFA
// @Description Remove the TOTP factor and recovery codes after proving a current TOTP or recovery code. The user's other sessions are revoked.
// @Tags Auth
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body dto.MFADisableRequest true "TOTP or recovery code"
// @Success 204
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "auth.mfa_not_enabled"
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/mfa/disable [post]
func (h *MFAHandler) Disable(c fiber.Ctx) error {
	var req dto.MFADisableRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	if err := h.mfa.Disable(c.Context(), httpx.AuthUserID(c), fiber.Locals[int64](c, middleware.LocalSessionID), req.Code); err != nil {
		return err
	}
	return response.NoContent(c)
}

// Regenerate godoc
// @Summary Regenerate recovery codes
// @Description Replace all recovery codes after proving a current TOTP or recovery code. The new codes are returned once and the old set stops working.
// @Tags Auth
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body dto.MFARegenerateRequest true "TOTP or recovery code"
// @Success 200 {object} dto.MFARecoveryCodesResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/mfa/recovery-codes/regenerate [post]
func (h *MFAHandler) Regenerate(c fiber.Ctx) error {
	var req dto.MFARegenerateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	codes, err := h.mfa.RegenerateRecoveryCodes(c.Context(), httpx.AuthUserID(c), req.Code)
	if err != nil {
		return err
	}
	return response.Success(c, dto.MFARecoveryCodesResponse{RecoveryCodes: codes})
}

// Status godoc
// @Summary MFA status
// @Description Return whether TOTP is enabled and how many recovery codes are unused.
// @Tags Auth
// @Security BearerAuth
// @Produce json
// @Success 200 {object} dto.MFAStatusResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/mfa/status [get]
func (h *MFAHandler) Status(c fiber.Ctx) error {
	status, err := h.mfa.Status(c.Context(), httpx.AuthUserID(c))
	if err != nil {
		return err
	}
	return response.Success(c, dto.MFAStatusResponse{
		Enabled:                status.Enabled,
		ConfirmedAt:            status.ConfirmedAt,
		RecoveryCodesRemaining: status.RecoveryCodesRemaining,
	})
}

// Verify godoc
// @Summary Complete MFA login
// @Description Exchange a two-step login challenge plus a TOTP or recovery code for the normal access and refresh token pair.
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.MFAVerifyRequest true "Challenge and MFA code"
// @Success 200 {object} dto.LoginResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/mfa/verify [post]
func (h *MFAHandler) Verify(c fiber.Ctx) error {
	var req dto.MFAVerifyRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	result, err := h.mfa.Verify(withClient(c), req.Challenge, req.Code)
	if err != nil {
		return err
	}
	user := toUserResponse(result.Member)
	fiber.Locals(c, "user_id", user.ID)
	return response.Success(c, dto.LoginResponse{
		AccessToken:  result.Tokens.AccessToken,
		RefreshToken: result.Tokens.RefreshToken,
		User:         user,
	})
}
