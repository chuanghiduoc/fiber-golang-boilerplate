package admin

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/app"
	adminhandler "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
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
	auth := deps.Authenticate()
	limit := deps.Limiters.Normal
	// The group carries no middleware on purpose: Fiber registers group
	// middleware as Use("/admin"), which would also run on /admin/features/*
	// and /admin/api-keys/* owned by other modules (double auth, double
	// rate-limit counting). Auth and the limiter are attached per route.
	adminGroup := v1.Group("/admin")
	adminGroup.Get("/stats", auth, limit, authzhttp.Require(authz.PermissionAdminStatsRead), m.admin.GetStats)
	adminGroup.Get("/users", auth, limit, authzhttp.Require(authz.PermissionAdminUsersRead), m.admin.ListUsers)
	adminGroup.Put("/users/:id/role", auth, limit, authzhttp.Require(authz.PermissionAdminUsersRole), deps.Idempotency, m.admin.UpdateRole)
	adminGroup.Post("/users/:id/ban", auth, limit, authzhttp.Require(authz.PermissionAdminUsersBan), deps.Idempotency, m.admin.BanUser)
	adminGroup.Post("/users/:id/unban", auth, limit, authzhttp.Require(authz.PermissionAdminUsersBan), deps.Idempotency, m.admin.UnbanUser)
	adminGroup.Get("/files", auth, limit, authzhttp.Require(authz.PermissionAdminFilesRead), m.admin.ListFiles)
	adminGroup.Get("/files/:id/download", auth, limit, authzhttp.Require(authz.PermissionAdminFilesRead), m.admin.DownloadFile)
	adminGroup.Delete("/files/:id", auth, limit, authzhttp.Require(authz.PermissionAdminFilesDelete), deps.Idempotency, m.admin.DeleteFile)
	adminGroup.Post("/files/:id/restore", auth, limit, authzhttp.Require(authz.PermissionAdminFilesRestore), deps.Idempotency, m.admin.RestoreFile)
}
