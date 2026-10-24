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

// Build constructs the files repository, services, and handler from infra.
// It performs no I/O.
func Build(infra app.Infra) (*Module, Provides) {
	cfg := infra.Config
	fileRepo := filesrepo.NewFileRepository(infra.Pool)
	uploadSvc := filesvc.NewUploadService(fileRepo, infra.Storage)
	// With a transaction manager, file mutations and their webhook outbox
	// jobs commit atomically (see AttachWebhooks).
	filesvc.AttachTxManager(uploadSvc, infra.Tx)
	uploadHandler := fileshandler.NewUploadHandler(uploadSvc, cfg.Storage.MaxFileSize, cfg.Storage.AllowedTypes())
	filesAdminSvc := filesvc.NewAdminFileService(fileRepo, infra.Tx, infra.Audit, infra.Storage)

	m := New(uploadHandler)
	m.uploadSvc = uploadSvc
	m.adminSvc = filesAdminSvc
	return m, Provides{Admin: filesadapter.NewAdminFiles(filesAdminSvc)}
}

// AttachWebhooks wires the outbound webhook dispatcher into user and admin
// upload/delete/restore. The webhooks module is built after files, so the
// composition root attaches it once both exist.
func (m *Module) AttachWebhooks(dispatcher webhookcontracts.Dispatcher) {
	if m.uploadSvc != nil {
		filesvc.AttachWebhookDispatcher(m.uploadSvc, dispatcher)
	}
	if m.adminSvc != nil {
		filesvc.AttachAdminWebhookDispatcher(m.adminSvc, dispatcher)
	}
}
