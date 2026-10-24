package admin

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module owns the back-office surface. It is deliberately thin; reporting and
// cross-module operations should move behind contracts over time.
type Module struct {
	admin *handler.AdminHandler
}

func New(admin *handler.AdminHandler) *Module {
	return &Module{admin: admin}
}

func (m *Module) Name() string { return "admin" }

func (m *Module) RegisterRoutes(v1 fiber.Router, deps app.Deps) {
	cfg := deps.Config
	adminGroup := v1.Group("/admin",
		middleware.JWTAuth(cfg.JWT.Secret, deps.Sessions),
		deps.Limiters.Normal,
	)
	adminGroup.Get("/stats", authz.Require(authz.PermissionAdminStatsRead), m.admin.GetStats)
	adminGroup.Get("/users", authz.Require(authz.PermissionAdminUsersRead), m.admin.ListUsers)
	adminGroup.Put("/users/:id/role", authz.Require(authz.PermissionAdminUsersRole), deps.Idempotency, m.admin.UpdateRole)
	adminGroup.Post("/users/:id/ban", authz.Require(authz.PermissionAdminUsersBan), deps.Idempotency, m.admin.BanUser)
	adminGroup.Post("/users/:id/unban", authz.Require(authz.PermissionAdminUsersBan), deps.Idempotency, m.admin.UnbanUser)
	adminGroup.Get("/files", authz.Require(authz.PermissionAdminFilesRead), m.admin.ListFiles)
	adminGroup.Get("/files/:id/download", authz.Require(authz.PermissionAdminFilesRead), m.admin.DownloadFile)
	adminGroup.Delete("/files/:id", authz.Require(authz.PermissionAdminFilesDelete), deps.Idempotency, m.admin.DeleteFile)
	adminGroup.Post("/files/:id/restore", authz.Require(authz.PermissionAdminFilesRestore), deps.Idempotency, m.admin.RestoreFile)
}
