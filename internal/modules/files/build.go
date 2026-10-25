package files

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	filesadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/adapter"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
)

// Provides lists the contracts files supplies to other modules.
type Provides struct {
	Admin filescontracts.FilesAdmin
}

// Settings are the upload limits files owns (STORAGE_MAX_FILE_SIZE and
// STORAGE_ALLOWED_MIME_TYPES). The storage driver itself is platform
// infrastructure (infra.Storage).
type Settings struct {
	MaxFileSize  int64
	AllowedTypes []string
}

// Build constructs the files repository, services, and handler from infra.
// It performs no I/O. Every file mutation publishes its domain event
// (file.uploaded/deleted/restored, internal/contracts/files) on infra.Events
// inside the mutation's transaction; subscribers such as webhooks never
// appear here.
func Build(infra app.Infra, settings Settings) (*Module, Provides) {
	fileRepo := filesrepo.NewFileRepository(infra.Pool)
	tx := filesrepo.NewTransactor(infra.Tx, infra.Audit, infra.Events)

	uploadSvc := filesvc.NewUploadService(fileRepo, tx, infra.Storage)
	uploadHandler := fileshandler.NewUploadHandler(uploadSvc, settings.MaxFileSize, settings.AllowedTypes)
	filesAdminSvc := filesvc.NewAdminFileService(fileRepo, tx, infra.Storage)

	return New(uploadHandler), Provides{Admin: filesadapter.NewAdminFiles(filesAdminSvc)}
}
