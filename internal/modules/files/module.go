package files

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module owns file upload, metadata, download, soft delete, and restore.
type Module struct {
	upload *fileshandler.UploadHandler
}

func New(upload *fileshandler.UploadHandler) *Module {
	return &Module{upload: upload}
}

func (m *Module) Name() string { return "files" }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	rl := deps.Limiters
	guard := func(action string) fiber.Handler {
		return audit.Guard(deps.Audit, audit.Policy{
			Mode:     audit.ModeRequired,
			Action:   action,
			Resource: "file",
		})
	}

	files := v1.Group("/files", middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions))
	// User file mutations are Tier 2: the pre-write intent row and completion
	// provide the durable trail even though the storage operation cannot join
	// the metadata transaction.
	files.Post("/upload", rl.Normal, deps.Idempotency, guard("file.uploaded"), m.upload.Upload)
	files.Get("/", rl.Relaxed, m.upload.List)
	files.Get("/:id/download", rl.Relaxed, m.upload.Download)
	files.Get("/:id", rl.Relaxed, m.upload.GetInfo)
	files.Delete("/:id", rl.Normal, deps.Idempotency, guard("file.deleted"), m.upload.Delete)
	files.Post("/:id/restore", rl.Normal, deps.Idempotency, guard("file.restored"), m.upload.Restore)
}
