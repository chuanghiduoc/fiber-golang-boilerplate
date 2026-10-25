package files

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	webhookcontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/webhooks"
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
// It performs no I/O. webhooks publishes file.uploaded/deleted/restored; when
// it can also enqueue inside a transaction (the webhooks module's
// dispatcher does), each file mutation and its outbox job commit atomically,
// otherwise events are dispatched after commit. A nil webhooks disables them.
func Build(infra app.Infra, settings Settings, webhooks webhookcontracts.Dispatcher) (*Module, Provides) {
	fileRepo := filesrepo.NewFileRepository(infra.Pool)
	outbox, _ := webhooks.(filesrepo.WebhookTxDispatcher)
	tx := filesrepo.NewTransactor(infra.Tx, infra.Audit, outbox)

	uploadSvc := filesvc.NewUploadService(fileRepo, tx, infra.Storage, webhooks)
	uploadHandler := fileshandler.NewUploadHandler(uploadSvc, settings.MaxFileSize, settings.AllowedTypes)
	filesAdminSvc := filesvc.NewAdminFileService(fileRepo, tx, infra.Storage, webhooks)

	return New(uploadHandler), Provides{Admin: filesadapter.NewAdminFiles(filesAdminSvc)}
}
