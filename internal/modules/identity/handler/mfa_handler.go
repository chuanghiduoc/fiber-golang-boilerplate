package handler

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// MFAHandler serves the authenticated MFA management endpoints and the public
// two-step login verification.
type MFAHandler struct {
	mfa           MFA
	keyring       *token.Keyring
	refreshCookie RefreshCookieConfig
}

func NewMFAHandler(mfa MFA) *MFAHandler {
	return &MFAHandler{mfa: mfa}
}

// SetJWTKeyring installs the keyring used to recover the current session id.
func (h *MFAHandler) SetJWTKeyring(keyring *token.Keyring) { h.keyring = keyring }

type mfaStatusInput struct {
	Authorization string `header:"Authorization"`
}

type mfaEnrollInput struct {
	Authorization string `header:"Authorization"`
	Body          dto.MFAEnrollRequest
}

type mfaConfirmInput struct {
	FactorID      string `path:"factorId" format:"uuid"`
	Authorization string `header:"Authorization"`
	Body          dto.MFAConfirmRequest
}

type mfaDisableInput struct {
	FactorID      string `path:"factorId" format:"uuid"`
	Authorization string `header:"Authorization"`
	Body          dto.MFADisableRequest
}

type mfaRegenerateInput struct {
	Authorization string `header:"Authorization"`
	Body          dto.MFARegenerateRequest
}

type mfaVerifyInput struct {
	Body dto.MFAVerifyRequest
}

type platformResetMFAInput struct {
	UserID string `path:"userId" format:"uuid"`
}

// Enroll starts or restarts a TOTP enrollment.
func (h *MFAHandler) Enroll(ctx context.Context, input *mfaEnrollInput) (*httpapi.Created[dto.MFAEnrollResponse], error) {
	enrollment, err := h.mfa.EnrollTOTP(ctx, actorID(ctx), sessionIDFromBearer(h.keyring, input.Authorization), input.Body.CurrentPassword)
	if err != nil {
		return nil, err
	}
	location := "/api/v1/me/mfa/totp/" + enrollment.FactorID.String()
	return httpapi.NewCreated(location, dto.MFAEnrollResponse{
		FactorID:   enrollment.FactorID,
		Type:       core.MFAFactorTOTP,
		OtpauthURI: enrollment.OtpauthURI,
	}), nil
}

// Confirm verifies the first code and enables the factor.
func (h *MFAHandler) Confirm(ctx context.Context, input *mfaConfirmInput) (*mfaConfirmOutput, error) {
	if _, err := parseUUID(input.FactorID, "factorId"); err != nil {
		return nil, err
	}
	result, err := h.mfa.ConfirmTOTP(ctx, actorID(ctx), sessionIDFromBearer(h.keyring, input.Authorization), input.Body.CurrentPassword, input.Body.Code)
	if err != nil {
		return nil, err
	}
	return &mfaConfirmOutput{
		SetCookie: h.refreshCookie.cookieHeader(result.Tokens.RefreshToken),
		Body: dto.MFAConfirmResponse{
			RecoveryCodes: result.RecoveryCodes,
			AccessToken:   result.Tokens.AccessToken,
			RefreshToken:  h.refreshCookie.jsonRefreshToken(result.Tokens.RefreshToken),
		},
	}, nil
}

// Disable removes the factor after a code proof.
func (h *MFAHandler) Disable(ctx context.Context, input *mfaDisableInput) (*httpapi.Empty, error) {
	if _, err := parseUUID(input.FactorID, "factorId"); err != nil {
		return nil, err
	}
	if err := h.mfa.Disable(ctx, actorID(ctx), sessionIDFromBearer(h.keyring, input.Authorization), input.Body.Code); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// Regenerate replaces the recovery codes after a code proof.
func (h *MFAHandler) Regenerate(ctx context.Context, input *mfaRegenerateInput) (*httpapi.Created[dto.MFARecoveryCodesResponse], error) {
	codes, err := h.mfa.RegenerateRecoveryCodes(ctx, actorID(ctx), input.Body.Code)
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated("", dto.MFARecoveryCodesResponse{RecoveryCodes: codes}), nil
}

// Status returns whether TOTP is enabled and how many recovery codes remain.
func (h *MFAHandler) Status(ctx context.Context, _ *mfaStatusInput) (*httpapi.Output[dto.MFAStatusResponse], error) {
	status, err := h.mfa.Status(ctx, actorID(ctx))
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.MFAStatusResponse]{Body: dto.MFAStatusResponse{
		Enabled:                status.Enabled,
		ConfirmedAt:            status.ConfirmedAt,
		RecoveryCodesRemaining: status.RecoveryCodesRemaining,
	}}, nil
}

// PlatformReset removes a user's MFA as a platform administrator.
func (h *MFAHandler) PlatformReset(ctx context.Context, input *platformResetMFAInput) (*httpapi.Empty, error) {
	id, err := parseUUID(input.UserID, "userId")
	if err != nil {
		return nil, err
	}
	if err := h.mfa.ResetTOTP(ctx, id); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// Verify completes a two-step sign-in.
func (h *MFAHandler) Verify(ctx context.Context, input *mfaVerifyInput) (*loginOutput, error) {
	result, err := h.mfa.Verify(withClientMetadata(ctx), input.Body.Challenge, input.Body.Code)
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
