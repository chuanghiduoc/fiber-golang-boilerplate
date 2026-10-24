package handler

import (
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
)

// Deprecated: use internal/modules/files/handler for new code.
type UploadHandler = fileshandler.UploadHandler

// Deprecated: use internal/modules/files/handler.NewUploadHandler for new code.
func NewUploadHandler(svc service.UploadService, maxFileSize int64, allowedTypes []string) *UploadHandler {
	return fileshandler.NewUploadHandler(svc, maxFileSize, allowedTypes)
}
