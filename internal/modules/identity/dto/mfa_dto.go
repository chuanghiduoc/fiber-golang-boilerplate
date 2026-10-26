package dto

import "time"

// MFAEnrollResponse is the only response that carries the TOTP secret, inside
// the otpauth provisioning URI.
type MFAEnrollResponse struct {
	Type       string `json:"type" enums:"totp"`
	OtpauthURI string `json:"otpauthUri"`
}

// MFAEnrollRequest re-authenticates the caller before a new factor is bound.
// CurrentPassword is required for an account with a local password; a
// Google-only account proves a recent interactive sign-in instead.
type MFAEnrollRequest struct {
	CurrentPassword string `json:"currentPassword" validate:"omitempty,max=128"`
}

type MFAConfirmRequest struct {
	Code string `json:"code" validate:"required,max=64"`
	// CurrentPassword re-authenticates the caller (password accounts).
	CurrentPassword string `json:"currentPassword" validate:"omitempty,max=128"`
}

// MFARecoveryCodesResponse is returned once, on confirmation and on
// regeneration; the codes are never retrievable again.
type MFARecoveryCodesResponse struct {
	RecoveryCodes []string `json:"recoveryCodes"`
}

// MFAConfirmResponse returns the recovery codes plus the re-issued token pair
// for the session that confirmed the factor. Confirmation bumps the token
// version, which invalidates every older session everywhere (including the
// previous access token of this one), so the caller must switch to the new
// pair to stay signed in. The pair is absent only when the confirming session
// stopped being live before the change committed; the caller is then logged
// out and must sign in again (with the second factor).
type MFAConfirmResponse struct {
	RecoveryCodes []string `json:"recoveryCodes"`
	AccessToken   string   `json:"accessToken,omitempty"`
	RefreshToken  string   `json:"refreshToken,omitempty"`
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

// MFAStepUpRequest refreshes the current session's second-factor freshness
// with a TOTP code or one of the single-use recovery codes.
type MFAStepUpRequest struct {
	Code string `json:"code" validate:"required,max=64"`
}

// MFAStepUpResponse reports when the second factor was verified; MFA-gated
// routes accept the session for MFA_MAX_AGE after this instant.
type MFAStepUpResponse struct {
	VerifiedAt time.Time `json:"verifiedAt"`
}
