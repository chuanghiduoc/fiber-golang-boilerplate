package handler

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// MFAStepUpHandler refreshes the current session's AAL2 freshness. It is
// JWT-only: an API key has no session whose mfa_verified_at it could update.
type MFAStepUpHandler struct {
	mfa service.MFAStepUpService
}

func NewMFAStepUpHandler(mfa service.MFAStepUpService) *MFAStepUpHandler {
	return &MFAStepUpHandler{mfa: mfa}
}

// StepUp godoc
// @Summary Refresh MFA freshness for the current session
// @Description Verify a current TOTP or recovery code and stamp the caller's session as freshly second-factor authenticated. Use it when an MFA-gated route returns 403 auth.mfa_required because the previous verification is older than MFA_MAX_AGE.
// @Tags Auth
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body dto.MFAStepUpRequest true "TOTP or recovery code"
// @Success 200 {object} dto.MFAStepUpResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "auth.mfa_not_enabled"
// @Failure 429 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /auth/mfa/step-up [post]
func (h *MFAStepUpHandler) StepUp(c fiber.Ctx) error {
	var req dto.MFAStepUpRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	verifiedAt, err := h.mfa.StepUp(
		c.Context(),
		httpx.AuthUserID(c),
		fiber.Locals[int64](c, middleware.LocalSessionID),
		req.Code,
	)
	if err != nil {
		return err
	}
	return response.Success(c, dto.MFAStepUpResponse{VerifiedAt: verifiedAt})
}
