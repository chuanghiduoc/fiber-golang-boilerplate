package dto

import (
	"time"

	"github.com/google/uuid"
)

// MFAEnrollResponse is the only response that carries the TOTP secret, inside
// the otpauth provisioning URI.
type MFAEnrollResponse struct {
	FactorID   uuid.UUID `json:"factorId"`
	Type       string    `json:"type" enum:"totp"`
	OtpauthURI string    `json:"otpauthUri"`
}

// MFAEnrollRequest re-authenticates the caller before a new factor is bound.
// CurrentPassword is required for an account with a local password; a
// Google-only account proves a recent interactive sign-in instead.
type MFAEnrollRequest struct {
	CurrentPassword string `json:"currentPassword,omitempty" maxLength:"128"`
}

type MFAConfirmRequest struct {
	Code string `json:"code" required:"true" maxLength:"64"`
	// CurrentPassword re-authenticates the caller (password accounts).
	CurrentPassword string `json:"currentPassword,omitempty" maxLength:"128"`
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
// pair to stay signed in.
type MFAConfirmResponse struct {
	RecoveryCodes []string `json:"recoveryCodes"`
	AccessToken   string   `json:"accessToken,omitempty"`
	RefreshToken  string   `json:"refreshToken,omitempty"`
}

type MFADisableRequest struct {
	Code string `json:"code" required:"true" maxLength:"64"`
}

type MFARegenerateRequest struct {
	Code string `json:"code" required:"true" maxLength:"64"`
}

type MFAStatusResponse struct {
	Enabled                bool       `json:"enabled"`
	ConfirmedAt            *time.Time `json:"confirmedAt,omitempty"`
	RecoveryCodesRemaining int64      `json:"recoveryCodesRemaining"`
}

// MFAVerifyRequest completes a two-step login. Code is a TOTP code or one of
// the single-use recovery codes.
type MFAVerifyRequest struct {
	Challenge string `json:"challenge" required:"true" maxLength:"4096"`
	Code      string `json:"code" required:"true" maxLength:"64"`
}

// MFAStepUpRequest refreshes the current session's second-factor freshness.
// Supply either a TOTP/recovery code or a passkey assertion; the assertion's
// challenge comes from POST /auth/passkeys/options.
type MFAStepUpRequest struct {
	Code    string             `json:"code,omitempty" maxLength:"64"`
	Passkey *PasskeyCredential `json:"passkey,omitempty"`
}

// MFAStepUpResponse reports when the second factor was verified; MFA-gated
// routes accept the session for MFA_MAX_AGE after this instant.
type MFAStepUpResponse struct {
	VerifiedAt time.Time `json:"verifiedAt"`
}
