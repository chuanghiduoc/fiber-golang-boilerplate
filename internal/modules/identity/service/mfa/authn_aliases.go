package mfa

import authn "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/authn"

type AccessClaims = authn.AccessClaims
type AccessTokenSigner = authn.AccessTokenSigner
type Admission = authn.Admission
type ChangePasswordInput = authn.ChangePasswordInput
type EmailVerificationService = authn.EmailVerificationService
type ForgotPasswordInput = authn.ForgotPasswordInput
type GoogleSignIn = authn.GoogleSignIn
type GoogleSignInResult = authn.GoogleSignInResult
type LoginInput = authn.LoginInput
type PasswordResetEmailLookup = authn.PasswordResetEmailLookup
type PasswordResetService = authn.PasswordResetService
type RefreshResult = authn.RefreshResult
type RegisterInput = authn.RegisterInput
type ResetDeps = authn.ResetDeps
type ResetPasswordInput = authn.ResetPasswordInput
type ResetRequestQueue = authn.ResetRequestQueue
type SecurityEvent = authn.SecurityEvent
type SecurityNotice = authn.SecurityNotice
type SecurityNotifier = authn.SecurityNotifier
type SessionService = authn.SessionService
type TokenPurger = authn.TokenPurger
type UpdateProfileInput = authn.UpdateProfileInput
type UserDeps = authn.UserDeps
type UserService = authn.UserService
type VerificationDeps = authn.VerificationDeps

const (
	ResetRequestJobKind                   = authn.ResetRequestJobKind
	SecurityEventEmailChanged             = authn.SecurityEventEmailChanged
	SecurityEventMFADisabled              = authn.SecurityEventMFADisabled
	SecurityEventMFAEnabled               = authn.SecurityEventMFAEnabled
	SecurityEventMFAReset                 = authn.SecurityEventMFAReset
	SecurityEventPasskeyAdded             = authn.SecurityEventPasskeyAdded
	SecurityEventPasskeyRemoved           = authn.SecurityEventPasskeyRemoved
	SecurityEventPasswordChanged          = authn.SecurityEventPasswordChanged
	SecurityEventPasswordReset            = authn.SecurityEventPasswordReset
	SecurityEventRecoveryCodesRegenerated = authn.SecurityEventRecoveryCodesRegenerated
	TokenPurgeInterval                    = authn.TokenPurgeInterval
	TokenPurgeJobKind                     = authn.TokenPurgeJobKind
)

const MaxMFAAttempts = authn.MaxMFAAttempts
