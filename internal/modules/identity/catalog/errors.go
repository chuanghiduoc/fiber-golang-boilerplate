package catalog

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Error codes owned by identity.
const (
	CodeAuthInvalidCredentials apperror.Code = "auth.invalid_credentials"
	CodeAuthEmailNotVerified   apperror.Code = "auth.email_not_verified"
	CodeAuthRefreshInvalid     apperror.Code = "auth.refresh_invalid"
	// CodeAuthRefreshConcurrent: a second refresh presented the same token
	// while the rotation was still inside the 10-second grace window. The
	// session stays valid; the caller should retry with the token it already
	// holds or the one from the winning response.
	CodeAuthRefreshConcurrent apperror.Code = "auth.refresh_concurrent"
	// CodeAuthCSRFFailed: a cookie-authenticated refresh/logout was not
	// same-origin and did not carry a matching Origin plus the custom CSRF
	// header.
	CodeAuthCSRFFailed apperror.Code = "auth.csrf_failed"
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
	// CodeAuthPasskeyInvalid: the WebAuthn ceremony is unknown, expired,
	// already used, belongs to another tenant/user, or the assertion failed.
	CodeAuthPasskeyInvalid apperror.Code = "auth.passkey_invalid"
	// CodeAuthPasskeyAlreadyRegistered: the credential id is already bound.
	CodeAuthPasskeyAlreadyRegistered apperror.Code = "auth.passkey_already_registered"
	// CodeAuthPasskeyNotFound: the caller has no passkey with that id.
	CodeAuthPasskeyNotFound apperror.Code = "auth.passkey_not_found"
	// CodeAuthPasskeySignCount: the authenticator's signature counter did not
	// advance, which is a clone / replay signal.
	CodeAuthPasskeySignCount apperror.Code = "auth.passkey_sign_count_invalid"
	// CodeAuthPasskeyNameInvalid: the requested passkey label is empty after
	// trimming or longer than 64 characters / contains control characters.
	CodeAuthPasskeyNameInvalid apperror.Code = "auth.passkey_name_invalid"
	// CodeAuthOAuthUpstreamUnavailable: Google's token or userinfo endpoint
	// failed in a way the client cannot fix (5xx, timeout, network).
	CodeAuthOAuthUpstreamUnavailable apperror.Code = "auth.oauth_upstream_unavailable"
	// CodeAuthOAuthCodeInvalid: the one-time Google exchange code is unknown,
	// already redeemed, expired, or bound to another tenant.
	CodeAuthOAuthCodeInvalid apperror.Code = "auth.oauth_code_invalid"
	// CodeAuthOAuthEmailNotVerified: Google authenticated the account but its
	// email address is unverified, so it cannot be linked or used to create a
	// local account.
	CodeAuthOAuthEmailNotVerified apperror.Code = "auth.oauth_email_not_verified"
	// CodeAuthOAuthUserInfoIncomplete: Google's userinfo response lacked the
	// stable subject id or the email address.
	CodeAuthOAuthUserInfoIncomplete apperror.Code = "auth.oauth_userinfo_incomplete"

	CodeUserEmailTaken apperror.Code = "user.email_taken"
	CodeUserNotFound   apperror.Code = "user.not_found"
	// CodeUserLastAdmin: removing the last active owner of an operational
	// tenant is refused by the protect_last_owner trigger and the service
	// pre-check.
	CodeUserLastAdmin apperror.Code = "user.last_admin"

	// CodeInvitationInvalid: unknown, expired, or already accepted
	// invitation token, or one addressed to another email.
	CodeInvitationInvalid apperror.Code = "invitation.invalid"
	// CodeInvitationInviterIneligible: the inviter no longer exists as an
	// active member who may grant the invitation's role.
	CodeInvitationInviterIneligible apperror.Code = "invitation.inviter_ineligible"
	// CodeMembershipExists: the invitee is already a member (or banned).
	CodeMembershipExists apperror.Code = "membership.exists"

	// CodeRoleInsufficientRank: the caller's tenant role rank is too low for
	// the role being granted or the member being managed.
	CodeRoleInsufficientRank apperror.Code = "role.insufficient_rank"

	// CodeTenantDeletionNotDue: a pending_deletion tenant is still inside its
	// deletion grace period.
	CodeTenantDeletionNotDue apperror.Code = "tenant.deletion_not_due"

	CodeDataExportInProgress apperror.Code = "data_export.in_progress"
	CodeDataExportNotFound   apperror.Code = "data_export.not_found"
	CodeDataExportNotReady   apperror.Code = "data_export.not_ready"
	CodeDataExportExpired    apperror.Code = "data_export.expired"
)

// Errors returns identity's error catalog for app.ErrorProvider.
func Errors() []apperror.Spec {
	return []apperror.Spec{
		{
			Code:    CodeAuthInvalidCredentials,
			Status:  http.StatusUnauthorized,
			Title:   "Invalid credentials",
			DocPath: "/errors/auth-invalid-credentials",
		},
		{
			Code:    CodeAuthEmailNotVerified,
			Status:  http.StatusForbidden,
			Title:   "Email not verified",
			DocPath: "/errors/auth-email-not-verified",
		},
		{
			Code:    CodeAuthRefreshInvalid,
			Status:  http.StatusUnauthorized,
			Title:   "Invalid refresh token",
			DocPath: "/errors/auth-refresh-invalid",
		},
		{
			Code:    CodeAuthRefreshConcurrent,
			Status:  http.StatusUnauthorized,
			Title:   "Concurrent refresh",
			DocPath: "/errors/auth-refresh-concurrent",
		},
		{
			Code:    CodeAuthCSRFFailed,
			Status:  http.StatusForbidden,
			Title:   "Cross-site request refused",
			DocPath: "/errors/auth-csrf-failed",
		},
		{
			Code:    CodeAuthMFAChallengeInvalid,
			Status:  http.StatusUnauthorized,
			Title:   "Invalid MFA challenge",
			DocPath: "/errors/auth-mfa-challenge-invalid",
		},
		{
			Code:    CodeAuthMFAInvalidCode,
			Status:  http.StatusUnauthorized,
			Title:   "Invalid MFA code",
			DocPath: "/errors/auth-mfa-invalid-code",
		},
		{
			Code:    CodeAuthMFAAlreadyEnabled,
			Status:  http.StatusConflict,
			Title:   "MFA is already enabled",
			DocPath: "/errors/auth-mfa-already-enabled",
		},
		{
			Code:    CodeAuthMFANotEnabled,
			Status:  http.StatusConflict,
			Title:   "MFA is not enabled",
			DocPath: "/errors/auth-mfa-not-enabled",
		},
		{
			Code:    CodeAuthMFAEnrollmentExpired,
			Status:  http.StatusConflict,
			Title:   "MFA enrollment has expired",
			DocPath: "/errors/auth-mfa-enrollment-expired",
		},
		{
			Code:    CodeAuthMFAReauthRequired,
			Status:  http.StatusForbidden,
			Title:   "Re-authentication required",
			DocPath: "/errors/auth-mfa-reauth-required",
		},
		{
			Code:    CodeAuthPasskeyInvalid,
			Status:  http.StatusUnauthorized,
			Title:   "Invalid passkey ceremony",
			DocPath: "/errors/auth-passkey-invalid",
		},
		{
			Code:    CodeAuthPasskeyAlreadyRegistered,
			Status:  http.StatusConflict,
			Title:   "Passkey is already registered",
			DocPath: "/errors/auth-passkey-already-registered",
		},
		{
			Code:    CodeAuthPasskeyNotFound,
			Status:  http.StatusNotFound,
			Title:   "Passkey not found",
			DocPath: "/errors/auth-passkey-not-found",
		},
		{
			Code:    CodeAuthPasskeySignCount,
			Status:  http.StatusUnauthorized,
			Title:   "Authenticator signature counter did not advance",
			DocPath: "/errors/auth-passkey-sign-count-invalid",
		},
		{
			Code:    CodeAuthPasskeyNameInvalid,
			Status:  http.StatusBadRequest,
			Title:   "Invalid passkey name",
			DocPath: "/errors/auth-passkey-name-invalid",
		},
		{
			Code:    CodeAuthOAuthUpstreamUnavailable,
			Status:  http.StatusBadGateway,
			Title:   "OAuth provider is unavailable",
			DocPath: "/errors/auth-oauth-upstream-unavailable",
		},
		{
			Code:    CodeAuthOAuthCodeInvalid,
			Status:  http.StatusUnauthorized,
			Title:   "Invalid Google sign-in code",
			DocPath: "/errors/auth-oauth-code-invalid",
		},
		{
			Code:    CodeAuthOAuthEmailNotVerified,
			Status:  http.StatusForbidden,
			Title:   "Google email address is not verified",
			DocPath: "/errors/auth-oauth-email-not-verified",
		},
		{
			Code:    CodeAuthOAuthUserInfoIncomplete,
			Status:  http.StatusBadRequest,
			Title:   "Incomplete Google user info",
			DocPath: "/errors/auth-oauth-userinfo-incomplete",
		},
		{
			Code:    CodeUserEmailTaken,
			Status:  http.StatusConflict,
			Title:   "Email already in use",
			DocPath: "/errors/user-email-taken",
		},
		{
			Code:    CodeUserNotFound,
			Status:  http.StatusNotFound,
			Title:   "User not found",
			DocPath: "/errors/user-not-found",
		},
		{
			Code:    CodeUserLastAdmin,
			Status:  http.StatusConflict,
			Title:   "Cannot remove the last owner",
			DocPath: "/errors/user-last-admin",
		},
		{
			Code:    CodeInvitationInvalid,
			Status:  http.StatusBadRequest,
			Title:   "Invalid invitation",
			DocPath: "/errors/invitation-invalid",
		},
		{
			Code:    CodeInvitationInviterIneligible,
			Status:  http.StatusConflict,
			Title:   "Invitation inviter cannot grant its role",
			DocPath: "/errors/invitation-inviter-ineligible",
		},
		{
			Code:    CodeMembershipExists,
			Status:  http.StatusConflict,
			Title:   "Already a member",
			DocPath: "/errors/membership-exists",
		},
		{
			Code:    CodeRoleInsufficientRank,
			Status:  http.StatusForbidden,
			Title:   "Insufficient role rank",
			DocPath: "/errors/role-insufficient-rank",
		},
		{
			Code:    CodeTenantDeletionNotDue,
			Status:  http.StatusConflict,
			Title:   "Tenant deletion is not due",
			DocPath: "/errors/tenant-deletion-not-due",
		},
		{
			Code:    CodeDataExportInProgress,
			Status:  http.StatusConflict,
			Title:   "A data export is already in progress",
			DocPath: "/errors/data-export-in-progress",
		},
		{
			Code:    CodeDataExportNotFound,
			Status:  http.StatusNotFound,
			Title:   "Data export not found",
			DocPath: "/errors/data-export-not-found",
		},
		{
			Code:    CodeDataExportNotReady,
			Status:  http.StatusConflict,
			Title:   "Data export is not ready",
			DocPath: "/errors/data-export-not-ready",
		},
		{
			Code:    CodeDataExportExpired,
			Status:  http.StatusGone,
			Title:   "Data export has expired",
			DocPath: "/errors/data-export-expired",
		},
	}
}
