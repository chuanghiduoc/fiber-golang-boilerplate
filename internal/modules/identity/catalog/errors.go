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
