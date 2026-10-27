package handler

import (
	"time"

	"context"
	"encoding/json"
	"github.com/go-webauthn/webauthn/protocol"
	"strings"

	"github.com/google/uuid"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/mfa"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/requestctx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// StartPasskeyRegistrationInput starts an authenticated registration ceremony.
type StartPasskeyRegistrationInput struct{}

// CreatePasskeyInput verifies a registration attestation. The body is the
// browser's PublicKeyCredential, optionally wrapped as {"name":...,"credential":...}.
type CreatePasskeyInput struct {
	Body json.RawMessage
}

// DeletePasskeyInput removes one of the caller's passkeys.
type DeletePasskeyInput struct {
	PasskeyID uuid.UUID `path:"passkeyId" doc:"Passkey id."`
}

// StartPasskeyLoginInput starts a login ceremony.
type StartPasskeyLoginInput struct {
	Body dto.PasskeyLoginOptionsRequest
}

// CompletePasskeyLoginInput verifies a login assertion. Like registration, the
// body may be the bare assertion or an envelope with a "credential" member.
type CompletePasskeyLoginInput struct {
	Body json.RawMessage
}

// StepUpPasskeyInput refreshes the current session's MFA freshness.
type StepUpPasskeyInput struct {
	Body dto.MFAStepUpRequest
}

// PasskeyManager is the passkey surface the passkey handlers use.
// *mfa.PasskeyService implements it.
type PasskeyManager interface {
	BeginRegistration(ctx context.Context, userID uuid.UUID) (*protocol.CredentialCreation, error)
	FinishRegistration(ctx context.Context, userID uuid.UUID, name string, raw []byte) (*core.Passkey, error)
	Delete(ctx context.Context, userID, passkeyID uuid.UUID) error
	BeginLogin(ctx context.Context, email string) (*protocol.CredentialAssertion, error)
	FinishLogin(ctx context.Context, raw []byte, admit mfa.Admission) (*mfa.PasskeyLogin, error)
	StepUpPasskey(ctx context.Context, userID, sessionID uuid.UUID, raw []byte) (time.Time, error)
}

// StepUp is the second-factor step-up surface.
// *mfa.MFAService implements it.
type StepUp interface {
	StepUp(ctx context.Context, userID, sessionID uuid.UUID, code string) (time.Time, error)
}

// PasskeyHandler serves passkey management, passkey login, and the passkey
// variant of the MFA step-up. Its operations are huma handlers like the rest of
// identity.
type PasskeyHandler struct {
	passkeys      PasskeyManager
	stepUp        StepUp
	auth          *AuthHandler
	refreshCookie RefreshCookieConfig
}

// NewPasskeyHandler wires the passkey operations. auth supplies the session
// issuer and the email-verification admission rule used by the other sign-in
// flows; it may be nil in focused tests.
func NewPasskeyHandler(passkeys PasskeyManager, stepUp StepUp, auth *AuthHandler) *PasskeyHandler {
	return &PasskeyHandler{passkeys: passkeys, stepUp: stepUp, auth: auth}
}

// StartRegistration returns the registration options for the signed-in user.
func (h *PasskeyHandler) StartRegistration(ctx context.Context, _ *StartPasskeyRegistrationInput) (*httpapi.Output[json.RawMessage], error) {
	if h.passkeys == nil {
		return nil, passkeysNotConfigured()
	}
	userID, err := passkeyUserID(ctx)
	if err != nil {
		return nil, err
	}
	creation, err := h.passkeys.BeginRegistration(ctx, userID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(creation)
	if err != nil {
		return nil, apperror.NewInternal("failed to encode passkey options")
	}
	return &httpapi.Output[json.RawMessage]{Body: raw}, nil
}

// Create verifies the attestation and stores the passkey.
func (h *PasskeyHandler) Create(ctx context.Context, input *CreatePasskeyInput) (*httpapi.Created[dto.PasskeyResponse], error) {
	if h.passkeys == nil {
		return nil, passkeysNotConfigured()
	}
	userID, err := passkeyUserID(ctx)
	if err != nil {
		return nil, err
	}
	var request dto.PasskeyRegistrationRequest
	if err := json.Unmarshal(input.Body, &request); err != nil {
		return nil, apperror.NewBadRequest("a passkey attestation is required")
	}
	raw, err := request.Credential.MarshalCredential()
	if err != nil || !passkeyCredentialPresent(request.Credential) {
		return nil, apperror.NewBadRequest("a passkey attestation is required")
	}
	passkey, err := h.passkeys.FinishRegistration(ctx, userID, request.Name, raw)
	if err != nil {
		return nil, err
	}
	response := toPasskeyResponse(passkey)
	return httpapi.NewCreated("/api/v1/me/mfa/passkeys/"+passkey.ID.String(), response), nil
}

// Delete removes one of the caller's passkeys. The row is global to the user,
// so another user's id behaves as not found.
func (h *PasskeyHandler) Delete(ctx context.Context, input *DeletePasskeyInput) (*httpapi.Empty, error) {
	if h.passkeys == nil {
		return nil, passkeysNotConfigured()
	}
	userID, err := passkeyUserID(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.passkeys.Delete(ctx, userID, input.PasskeyID); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// StartLogin returns a discoverable passkey assertion challenge. An email
// hint is accepted but never changes the response, so it cannot enumerate
// accounts.
func (h *PasskeyHandler) StartLogin(ctx context.Context, input *StartPasskeyLoginInput) (*httpapi.Output[json.RawMessage], error) {
	if h.passkeys == nil {
		return nil, passkeysNotConfigured()
	}
	assertion, err := h.passkeys.BeginLogin(ctx, input.Body.Email)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(assertion)
	if err != nil {
		return nil, apperror.NewInternal("failed to encode passkey options")
	}
	return &httpapi.Output[json.RawMessage]{Body: raw}, nil
}

// CompleteLogin verifies the assertion and issues a normal token pair. A
// passkey is a phishing-resistant second factor, so the response never asks for
// a TOTP challenge.
func (h *PasskeyHandler) CompleteLogin(ctx context.Context, input *CompletePasskeyLoginInput) (*loginOutput, error) {
	if h.passkeys == nil {
		return nil, passkeysNotConfigured()
	}
	var request dto.PasskeyLoginVerificationRequest
	if err := json.Unmarshal(input.Body, &request); err != nil {
		return nil, apperror.NewBadRequest("a passkey assertion is required")
	}
	raw, err := request.Credential.MarshalCredential()
	if err != nil || !passkeyCredentialPresent(request.Credential) {
		return nil, apperror.NewBadRequest("a passkey assertion is required")
	}
	result, err := h.passkeys.FinishLogin(ctx, raw, h.admit)
	if err != nil {
		return nil, err
	}
	user := toUserResponse(result.Member)
	return &loginOutput{
		SetCookie: h.refreshCookie.cookieHeader(result.Tokens.RefreshToken),
		Body: dto.LoginResponse{
			AccessToken:  result.Tokens.AccessToken,
			RefreshToken: h.refreshCookie.jsonRefreshToken(result.Tokens.RefreshToken),
			User:         user,
		},
	}, nil
}

// StepUp refreshes the current session's AAL2 clock with either a TOTP /
// recovery code or a passkey assertion. A passkey also adds hwk/swk to the
// session's amr; the response reports the new verification time.
func (h *PasskeyHandler) StepUp(ctx context.Context, input *StepUpPasskeyInput) (*httpapi.Output[dto.MFAStepUpResponse], error) {
	userID, err := passkeyUserID(ctx)
	if err != nil {
		return nil, err
	}
	sessionID := requestctx.SessionID(ctx)
	if sessionID == uuid.Nil {
		return nil, apperror.NewUnauthorized("session is required")
	}
	hasCode := strings.TrimSpace(input.Body.Code) != ""
	hasPasskey := input.Body.Passkey != nil
	switch {
	case hasCode && hasPasskey:
		return nil, apperror.NewBadRequest("provide either a code or a passkey assertion, not both")
	case hasPasskey:
		if h.passkeys == nil {
			return nil, passkeysNotConfigured()
		}
		raw, err := input.Body.Passkey.MarshalCredential()
		if err != nil || !passkeyCredentialPresent(*input.Body.Passkey) {
			return nil, apperror.NewBadRequest("a passkey assertion is required")
		}
		verifiedAt, err := h.passkeys.StepUpPasskey(ctx, userID, sessionID, raw)
		if err != nil {
			return nil, err
		}
		return &httpapi.Output[dto.MFAStepUpResponse]{Body: dto.MFAStepUpResponse{VerifiedAt: verifiedAt}}, nil
	case hasCode:
		if h.stepUp == nil {
			return nil, apperror.NewCode(apperror.CodeSystemUnavailable, "MFA is not configured")
		}
		verifiedAt, err := h.stepUp.StepUp(ctx, userID, sessionID, input.Body.Code)
		if err != nil {
			return nil, err
		}
		return &httpapi.Output[dto.MFAStepUpResponse]{Body: dto.MFAStepUpResponse{VerifiedAt: verifiedAt}}, nil
	default:
		return nil, apperror.NewBadRequest("a code or passkey assertion is required")
	}
}

// admit applies the same email-verification rule as password and Google
// sign-in. It runs before the session is issued, so an unverified account
// cannot obtain tokens through a passkey.
func (h *PasskeyHandler) admit(ctx context.Context, member *core.Member) error {
	if h.auth == nil {
		return nil
	}
	if !member.User.EmailVerified() && h.auth.emailVerificationRequired(ctx, member) {
		return apperror.NewCode(identitycatalog.CodeAuthEmailNotVerified, "email not verified")
	}
	return nil
}

// passkeyUserID returns the authenticated user of a JWT-only operation.
func passkeyUserID(ctx context.Context) (uuid.UUID, error) {
	subject, ok := authz.SubjectFromContext(ctx)
	if !ok || subject.UserID == uuid.Nil || subject.TenantID == uuid.Nil {
		return uuid.Nil, apperror.NewUnauthorized("authentication is required")
	}
	return subject.UserID, nil
}

func passkeyCredentialPresent(credential dto.PasskeyCredential) bool {
	return credential.ID != "" && credential.RawID != "" && len(credential.Response) > 0
}

func passkeysNotConfigured() error {
	return apperror.NewCode(apperror.CodeSystemUnavailable, "passkeys are not configured")
}

func toPasskeyResponse(passkey *core.Passkey) dto.PasskeyResponse {
	transports := passkey.Transports
	if transports == nil {
		transports = []string{}
	}
	return dto.PasskeyResponse{
		ID:             passkey.ID,
		Name:           passkey.Name,
		Transports:     transports,
		BackupEligible: passkey.BackupEligible,
		BackupState:    passkey.BackupState,
		CreatedAt:      passkey.CreatedAt,
		LastUsedAt:     passkey.LastUsedAt,
	}
}
