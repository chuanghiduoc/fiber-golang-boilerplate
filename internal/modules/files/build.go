package files

import (
	"fmt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	filesadapter "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/adapter"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesrepo "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/repository"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/jobs"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/scheduler"
)

// Provides lists the contracts files supplies to other modules.
type Provides struct {
	Admin filescontracts.FilesAdmin
}

// Settings are the upload limits files owns (STORAGE_MAX_FILE_SIZE and
// STORAGE_ALLOWED_MIME_TYPES) and the trash retention
// (FILES_TRASH_RETENTION_DAYS). The storage driver itself is platform
// infrastructure (infra.Storage).
type Settings struct {
	MaxFileSize  int64
	AllowedTypes []string
	// TrashRetentionDays is how long a soft-deleted file stays restorable
	// before files.trash.purge removes its blob and row. Zero disables the
	// purge (tests); config validates 1..3650.
	TrashRetentionDays int32
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

	m := New(uploadHandler)
	if settings.TrashRetentionDays > 0 {
		m.trashPurger = filesvc.NewTrashPurger(filesrepo.NewTrashRepository(infra.Tx), infra.Storage, settings.TrashRetentionDays)
	}
	return m, Provides{Admin: filesadapter.NewAdminFiles(filesAdminSvc)}
}

// RegisterJobs implements app.JobProvider: the worker runs the trash purge
// (under authz.SystemContext, via the app's system registrar). A module built
// with New (tests) registers nothing.
func (m *Module) RegisterJobs(registrar jobs.Registrar) {
	if m.trashPurger != nil {
		registrar.Register(filesvc.TrashPurgeJobKind, m.trashPurger.HandleJob)
	}
}

// trashPurgeSchedule enqueues the trash purge. The scheduler is optional
// (SCHEDULER_ENABLED, default false); without it trashed files stay
// restorable and keep their blobs indefinitely.
var trashPurgeSchedule = scheduler.Spec{
	Name:     "files.trash.purge",
	Interval: filesvc.TrashPurgeInterval,
	Kind:     filesvc.TrashPurgeJobKind,
}

// RegisterSchedules implements app.ScheduleProvider. The spec is a constant,
// so a registration error is a programming error and fails startup loudly.
func (m *Module) RegisterSchedules(registry *scheduler.Registry) {
	if m.trashPurger == nil {
		return
	}
	if err := registry.Register(trashPurgeSchedule); err != nil {
		panic(fmt.Sprintf("files: register %s schedule: %v", trashPurgeSchedule.Name, err))
	}
}
