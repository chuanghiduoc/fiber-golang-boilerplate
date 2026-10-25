package dto

import "time"

// MFAEnrollResponse is the only response that carries the TOTP secret, inside
// the otpauth provisioning URI.
type MFAEnrollResponse struct {
	Type       string `json:"type" enums:"totp"`
	OtpauthURI string `json:"otpauthUri"`
}

type MFAConfirmRequest struct {
	Code string `json:"code" validate:"required,max=64"`
}

// MFARecoveryCodesResponse is returned once, on confirmation and on
// regeneration; the codes are never retrievable again.
type MFARecoveryCodesResponse struct {
	RecoveryCodes []string `json:"recoveryCodes"`
}

type MFADisableRequest struct {
	Code string `json:"code" validate:"required,max=64"`
}

type MFARegenerateRequest struct {
	Code string `json:"code" validate:"required,max=64"`
}

type MFAStatusResponse struct {
	Enabled                bool       `json:"enabled"`
	ConfirmedAt            *time.Time `json:"confirmedAt,omitempty"`
	RecoveryCodesRemaining int64      `json:"recoveryCodesRemaining"`
}

// MFAVerifyRequest completes a two-step login. Code is a TOTP code or one of
// the single-use recovery codes.
type MFAVerifyRequest struct {
	Challenge string `json:"challenge" validate:"required,max=4096"`
	Code      string `json:"code" validate:"required,max=64"`
}
