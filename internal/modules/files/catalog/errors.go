package catalog

import (
	"net/http"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Error codes owned by files.
const (
	CodeFileTooLarge        apperror.Code = "file.too_large"
	CodeFileUnsupportedType apperror.Code = "file.unsupported_type"
	CodeFileNotDeleted      apperror.Code = "file.not_deleted"
	CodeFileQuotaExceeded   apperror.Code = "file.quota_exceeded"
	CodeFileInfected        apperror.Code = "file.infected"
	CodeFileScanUnavailable apperror.Code = "file.scan_unavailable"
)

// Errors returns the files error catalog for app.ErrorProvider.
func Errors() []apperror.Spec {
	return []apperror.Spec{
		{
			Code:    CodeFileTooLarge,
			Status:  http.StatusRequestEntityTooLarge,
			Title:   "File too large",
			DocPath: "/errors/file-too-large",
		},
		{
			Code:    CodeFileUnsupportedType,
			Status:  http.StatusUnsupportedMediaType,
			Title:   "Unsupported file type",
			DocPath: "/errors/file-unsupported-type",
		},
		{
			Code:    CodeFileNotDeleted,
			Status:  http.StatusConflict,
			Title:   "File was not deleted",
			DocPath: "/errors/file-not-deleted",
		},
		{
			Code:    CodeFileQuotaExceeded,
			Status:  http.StatusRequestEntityTooLarge,
			Title:   "Storage quota exceeded",
			DocPath: "/errors/file-quota-exceeded",
		},
		{
			Code:    CodeFileInfected,
			Status:  http.StatusUnprocessableEntity,
			Title:   "File failed the malware scan",
			DocPath: "/errors/file-infected",
		},
		{
			Code:    CodeFileScanUnavailable,
			Status:  http.StatusServiceUnavailable,
			Title:   "File scanning is unavailable",
			DocPath: "/errors/file-scan-unavailable",
		},
	}
}
