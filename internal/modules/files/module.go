package files

import (
	"io/fs"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	filescatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/catalog"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
)

// Module owns file upload, metadata, download, soft delete, and restore.
type Module struct {
	upload *fileshandler.UploadHandler
	// uploadSvc and adminSvc are set by Build so AttachWebhooks can wire the
	// dispatcher.
	uploadSvc filesvc.UploadService
	adminSvc  filesvc.AdminFileService
}

// New builds the module from its handler. Production uses Build.
func New(upload *fileshandler.UploadHandler) *Module {
	return &Module{upload: upload}
}

func (m *Module) Name() string { return filescatalog.Owner }

// Permissions implements app.PermissionProvider.
func (m *Module) Permissions() []authz.Grant { return filescatalog.Permissions() }

// ErrorCodes implements app.ErrorProvider.
func (m *Module) ErrorCodes() map[apperror.Code]apperror.Spec { return filescatalog.ErrorCodes() }

// FeatureFlags implements app.FlagProvider.
func (m *Module) FeatureFlags() []features.Definition { return filescatalog.FeatureFlags() }

// Locales implements app.LocaleProvider.
func (m *Module) Locales() fs.FS { return filescatalog.Locales() }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	rl := deps.Limiters
	guard := func(action string) fiber.Handler {
		return audit.Guard(deps.Audit, audit.Policy{
			Mode:     audit.ModeRequired,
			Action:   action,
			Resource: "file",
		})
	}
	files := v1.Group("/files", deps.Authenticate())
	// User file mutations are Tier 2: the pre-write intent row and completion
	// provide the durable trail even though the storage operation cannot join
	// the metadata transaction.
	// The file_uploads gate runs after auth (so user targets apply) and before
	// idempotency/audit, so a disabled upload writes no intent row.
	files.Post("/upload", rl.Normal, authz.Require(filescatalog.PermissionFilesWrite), features.Middleware(deps.Flags, filescatalog.KeyFileUploads), deps.Idempotency, guard("file.uploaded"), m.upload.Upload)
	files.Get("/", rl.Relaxed, authz.Require(filescatalog.PermissionFilesRead), m.upload.List)
	files.Get("/:id/download", rl.Relaxed, authz.Require(filescatalog.PermissionFilesRead), m.upload.Download)
	files.Get("/:id", rl.Relaxed, authz.Require(filescatalog.PermissionFilesRead), m.upload.GetInfo)
	files.Delete("/:id", rl.Normal, authz.Require(filescatalog.PermissionFilesDelete), deps.Idempotency, guard("file.deleted"), m.upload.Delete)
	files.Post("/:id/restore", rl.Normal, authz.Require(filescatalog.PermissionFilesRestore), deps.Idempotency, guard("file.restored"), m.upload.Restore)
}
