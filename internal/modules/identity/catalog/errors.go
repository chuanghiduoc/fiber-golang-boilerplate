package catalog

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Error codes owned by identity. user.last_admin stays in pkg/apperror until
// the identity repository that returns it is migrated to this catalog.
const (
	CodeAuthInvalidCredentials apperror.Code = "auth.invalid_credentials"
	CodeAuthEmailNotVerified   apperror.Code = "auth.email_not_verified"
	CodeAuthRefreshInvalid     apperror.Code = "auth.refresh_invalid"
	// CodeAuthMFAChallengeInvalid: the challenge expired, was already used,
	// or belongs to another tenant.
	CodeAuthMFAChallengeInvalid apperror.Code = "auth.mfa_challenge_invalid"
	// CodeAuthMFAInvalidCode: a TOTP or recovery code did not match.
	CodeAuthMFAInvalidCode apperror.Code = "auth.mfa_invalid_code"
	// CodeAuthMFAAlreadyEnabled: a confirmed factor already exists.
	CodeAuthMFAAlreadyEnabled apperror.Code = "auth.mfa_already_enabled"
	// CodeAuthMFANotEnabled: no confirmed factor exists.
	CodeAuthMFANotEnabled apperror.Code = "auth.mfa_not_enabled"
	// CodeAuthMFAEnrollmentExpired: the pending enrollment is older than its
	// 15-minute window; start a new one.
	CodeAuthMFAEnrollmentExpired apperror.Code = "auth.mfa_enrollment_expired"
	// CodeAuthMFAReauthRequired: a passwordless account must sign in again
	// (the session is older than the re-authentication window).
	CodeAuthMFAReauthRequired apperror.Code = "auth.mfa_reauth_required"
	// CodeAuthOAuthUpstreamUnavailable: Google's token or userinfo endpoint
	// failed in a way the client cannot fix (5xx, timeout, network).
	CodeAuthOAuthUpstreamUnavailable apperror.Code = "auth.oauth_upstream_unavailable"

	CodeUserEmailTaken apperror.Code = "user.email_taken"
	CodeUserNotFound   apperror.Code = "user.not_found"

	// CodeInvitationInvalid: unknown, expired, or already accepted
	// invitation token, or one addressed to another email.
	CodeInvitationInvalid apperror.Code = "invitation.invalid"
	// CodeMembershipExists: the invitee is already a member (or banned).
	CodeMembershipExists apperror.Code = "membership.exists"

	CodeDataExportInProgress apperror.Code = "data_export.in_progress"
	CodeDataExportNotFound   apperror.Code = "data_export.not_found"
	CodeDataExportNotReady   apperror.Code = "data_export.not_ready"
	CodeDataExportExpired    apperror.Code = "data_export.expired"
)

// ErrorCodes returns identity's error catalog for app.ErrorProvider.
func ErrorCodes() map[apperror.Code]apperror.Spec {
	return map[apperror.Code]apperror.Spec{
		CodeAuthInvalidCredentials: {
			Status:  http.StatusUnauthorized,
			Title:   "Invalid credentials",
			DocPath: "/errors/auth-invalid-credentials",
		},
		CodeAuthEmailNotVerified: {
			Status:  http.StatusForbidden,
			Title:   "Email not verified",
			DocPath: "/errors/auth-email-not-verified",
		},
		CodeAuthRefreshInvalid: {
			Status:  http.StatusUnauthorized,
			Title:   "Invalid refresh token",
			DocPath: "/errors/auth-refresh-invalid",
		},
		CodeAuthMFAChallengeInvalid: {
			Status:  http.StatusUnauthorized,
			Title:   "Invalid MFA challenge",
			DocPath: "/errors/auth-mfa-challenge-invalid",
		},
		CodeAuthMFAInvalidCode: {
			Status:  http.StatusUnauthorized,
			Title:   "Invalid MFA code",
			DocPath: "/errors/auth-mfa-invalid-code",
		},
		CodeAuthMFAAlreadyEnabled: {
			Status:  http.StatusConflict,
			Title:   "MFA is already enabled",
			DocPath: "/errors/auth-mfa-already-enabled",
		},
		CodeAuthMFANotEnabled: {
			Status:  http.StatusConflict,
			Title:   "MFA is not enabled",
			DocPath: "/errors/auth-mfa-not-enabled",
		},
		CodeAuthMFAEnrollmentExpired: {
			Status:  http.StatusConflict,
			Title:   "MFA enrollment has expired",
			DocPath: "/errors/auth-mfa-enrollment-expired",
		},
		CodeAuthMFAReauthRequired: {
			Status:  http.StatusForbidden,
			Title:   "Re-authentication required",
			DocPath: "/errors/auth-mfa-reauth-required",
		},
		CodeAuthOAuthUpstreamUnavailable: {
			Status:  http.StatusBadGateway,
			Title:   "OAuth provider is unavailable",
			DocPath: "/errors/auth-oauth-upstream-unavailable",
		},
		CodeUserEmailTaken: {
			Status:  http.StatusConflict,
			Title:   "Email already in use",
			DocPath: "/errors/user-email-taken",
		},
		CodeUserNotFound: {
			Status:  http.StatusNotFound,
			Title:   "User not found",
			DocPath: "/errors/user-not-found",
		},
		CodeInvitationInvalid: {
			Status:  http.StatusBadRequest,
			Title:   "Invalid invitation",
			DocPath: "/errors/invitation-invalid",
		},
		CodeMembershipExists: {
			Status:  http.StatusConflict,
			Title:   "Already a member",
			DocPath: "/errors/membership-exists",
		},
		CodeDataExportInProgress: {
			Status:  http.StatusConflict,
			Title:   "A data export is already in progress",
			DocPath: "/errors/data-export-in-progress",
		},
		CodeDataExportNotFound: {
			Status:  http.StatusNotFound,
			Title:   "Data export not found",
			DocPath: "/errors/data-export-not-found",
		},
		CodeDataExportNotReady: {
			Status:  http.StatusConflict,
			Title:   "Data export is not ready",
			DocPath: "/errors/data-export-not-ready",
		},
		CodeDataExportExpired: {
			Status:  http.StatusGone,
			Title:   "Data export has expired",
			DocPath: "/errors/data-export-expired",
		},
	}
}
