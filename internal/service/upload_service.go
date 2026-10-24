package service

import (
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

// Deprecated: use internal/modules/files/service for new code.
type UploadService = filesvc.UploadService

// Deprecated: use internal/modules/files/service.NewUploadService for new code.
func NewUploadService(repo repository.FileRepository, store storage.Storage) UploadService {
	return filesvc.NewUploadService(repo, store)
}
