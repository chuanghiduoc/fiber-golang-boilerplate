package files

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	fileshandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/handler"
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

	files := v1.Group("/files", middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions))
	files.Post("/upload", rl.Normal, deps.Idempotency, m.upload.Upload)
	files.Get("/", rl.Relaxed, m.upload.List)
	files.Get("/:id/download", rl.Relaxed, m.upload.Download)
	files.Get("/:id", rl.Relaxed, m.upload.GetInfo)
	files.Delete("/:id", rl.Normal, deps.Idempotency, m.upload.Delete)
	files.Post("/:id/restore", rl.Normal, deps.Idempotency, m.upload.Restore)
}
