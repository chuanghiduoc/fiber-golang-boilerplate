package router

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/config"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/handler"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/health"
)

type Deps struct {
	AuthHandler   *handler.AuthHandler
	UserHandler   *handler.UserHandler
	UploadHandler *handler.UploadHandler
	AdminHandler  *handler.AdminHandler
	Config        *config.Config
	Pool          *pgxpool.Pool
	Health        *health.Checker
}
