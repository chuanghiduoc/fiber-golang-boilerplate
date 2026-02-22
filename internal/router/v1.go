package router

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/middleware"
)

func RegisterV1Routes(v1 fiber.Router, deps Deps) {
	cfg := deps.Config

	// Rate limiters (tiered)
	rl := cfg.RateLimit
	strictLimiter := middleware.NewLimiter(rl.StrictMax, rl.StrictWindow)
	normalLimiter := middleware.NewLimiter(rl.NormalMax, rl.NormalWindow)
	relaxedLimiter := middleware.NewLimiter(rl.RelaxedMax, rl.RelaxedWindow)

	// Auth routes (public)
	auth := v1.Group("/auth")
	auth.Post("/register", strictLimiter, deps.AuthHandler.Register)
	auth.Post("/login", strictLimiter, deps.AuthHandler.Login)
	auth.Post("/refresh", normalLimiter, deps.AuthHandler.Refresh)
	auth.Post("/logout", normalLimiter, deps.AuthHandler.Logout)
	auth.Post("/forgot-password", strictLimiter, deps.AuthHandler.ForgotPassword)
	auth.Post("/reset-password", strictLimiter, deps.AuthHandler.ResetPassword)
	auth.Post("/verify-email", normalLimiter, deps.AuthHandler.VerifyEmail)
	auth.Post("/resend-verification", normalLimiter, deps.AuthHandler.ResendVerification)
	auth.Get("/google", normalLimiter, deps.AuthHandler.GoogleRedirect)
	auth.Get("/google/callback", normalLimiter, deps.AuthHandler.GoogleCallback)

	// User routes (protected)
	users := v1.Group("/users", middleware.JWTAuth(cfg.JWT.Secret))
	users.Get("/me", relaxedLimiter, deps.UserHandler.GetMe)
	users.Put("/me", normalLimiter, deps.UserHandler.UpdateMe)
	users.Put("/me/password", normalLimiter, deps.UserHandler.ChangePassword)
	users.Get("/:id", relaxedLimiter, deps.UserHandler.GetByID)
	users.Get("/", relaxedLimiter, middleware.RequireRole(dto.RoleAdmin), deps.UserHandler.List)
	users.Put("/:id", normalLimiter, deps.UserHandler.Update)
	users.Delete("/:id", normalLimiter, deps.UserHandler.Delete)

	// File routes (protected)
	files := v1.Group("/files", middleware.JWTAuth(cfg.JWT.Secret))
	files.Post("/upload", normalLimiter, deps.UploadHandler.Upload)
	files.Get("/", relaxedLimiter, deps.UploadHandler.List)
	files.Get("/:id", relaxedLimiter, deps.UploadHandler.GetInfo)
	files.Get("/:id/download", relaxedLimiter, deps.UploadHandler.Download)
	files.Delete("/:id", normalLimiter, deps.UploadHandler.Delete)

	// Admin routes (protected, admin-only)
	admin := v1.Group("/admin",
		middleware.JWTAuth(cfg.JWT.Secret),
		middleware.RequireRole(dto.RoleAdmin),
		normalLimiter,
	)
	admin.Get("/stats", deps.AdminHandler.GetStats)
	admin.Get("/users", deps.AdminHandler.ListUsers)
	admin.Put("/users/:id/role", deps.AdminHandler.UpdateRole)
	admin.Post("/users/:id/ban", deps.AdminHandler.BanUser)
	admin.Post("/users/:id/unban", deps.AdminHandler.UnbanUser)
	admin.Get("/files", deps.AdminHandler.ListFiles)
}
