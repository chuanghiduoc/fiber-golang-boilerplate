package admin

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
)

// Module owns the back-office surface. Its services compose narrow
// identity/files contracts and never import those modules' internals.
type Module struct {
	admin *adminhandler.Handler
}

func New(admin *adminhandler.Handler) *Module {
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
