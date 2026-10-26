package catalog

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Error codes owned by files.
const (
	CodeFileNotFound        apperror.Code = "file.not_found"
	CodeFileTooLarge        apperror.Code = "file.too_large"
	CodeFileUnsupportedType apperror.Code = "file.unsupported_type"
	CodeFileNotDeleted      apperror.Code = "file.not_deleted"
	CodeFileQuotaExceeded   apperror.Code = "file.quota_exceeded"
	CodeFileInfected        apperror.Code = "file.infected"
	CodeFileScanUnavailable apperror.Code = "file.scan_unavailable"
)

// ErrorCodes returns the files error catalog for app.ErrorProvider.
func ErrorCodes() map[apperror.Code]apperror.Spec {
	return map[apperror.Code]apperror.Spec{
		CodeFileNotFound: {
			Status:  http.StatusNotFound,
			Title:   "File not found",
			DocPath: "/errors/file-not-found",
		},
		CodeFileTooLarge: {
			Status:  http.StatusRequestEntityTooLarge,
			Title:   "File too large",
			DocPath: "/errors/file-too-large",
		},
		CodeFileUnsupportedType: {
			Status:  http.StatusUnsupportedMediaType,
			Title:   "Unsupported file type",
			DocPath: "/errors/file-unsupported-type",
		},
		CodeFileNotDeleted: {
			Status:  http.StatusConflict,
			Title:   "File was not deleted",
			DocPath: "/errors/file-not-deleted",
		},
		CodeFileQuotaExceeded: {
			Status:  http.StatusRequestEntityTooLarge,
			Title:   "Storage quota exceeded",
			DocPath: "/errors/file-quota-exceeded",
		},
		CodeFileInfected: {
			Status:  http.StatusUnprocessableEntity,
			Title:   "File failed the malware scan",
			DocPath: "/errors/file-infected",
		},
		CodeFileScanUnavailable: {
			Status:  http.StatusServiceUnavailable,
			Title:   "File scanning is unavailable",
			DocPath: "/errors/file-scan-unavailable",
		},
	}
}
