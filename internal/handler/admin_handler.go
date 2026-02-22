package handler

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

type AdminHandler struct {
	service service.AdminService
}

func NewAdminHandler(svc service.AdminService) *AdminHandler {
	return &AdminHandler{service: svc}
}

// GetStats godoc
// @Summary Get system statistics
// @Description Get system-wide statistics (admin only)
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Success 200 {object} response.Response{data=dto.AdminStatsResponse}
// @Failure 401 {object} response.Response
// @Failure 403 {object} response.Response
// @Router /admin/stats [get]
func (h *AdminHandler) GetStats(c fiber.Ctx) error {
	stats, err := h.service.GetStats(c.Context())
	if err != nil {
		return err
	}

	return response.Success(c, stats)
}

// ListUsers godoc
// @Summary List all users (admin)
// @Description Get a paginated list of all users including soft-deleted
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param page query int false "Page number" default(1)
// @Param per_page query int false "Items per page" default(10)
// @Success 200 {object} response.Response{data=[]dto.UserResponse,meta=response.Meta}
// @Failure 401 {object} response.Response
// @Failure 403 {object} response.Response
// @Router /admin/users [get]
func (h *AdminHandler) ListUsers(c fiber.Ctx) error {
	page, perPage, err := paginationQuery(c)
	if err != nil {
		return err
	}

	users, total, err := h.service.ListUsers(c.Context(), page, perPage)
	if err != nil {
		return err
	}

	return response.SuccessWithMeta(c, users, response.NewMeta(page, perPage, total))
}

// UpdateRole godoc
// @Summary Update user role
// @Description Update a user's role (admin only)
// @Tags Admin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Param request body dto.UpdateRoleRequest true "Role update request"
// @Success 200 {object} response.Response{data=dto.UserResponse}
// @Failure 400 {object} response.Response
// @Failure 401 {object} response.Response
// @Failure 403 {object} response.Response
// @Failure 404 {object} response.Response
// @Router /admin/users/{id}/role [put]
func (h *AdminHandler) UpdateRole(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	var req dto.UpdateRoleRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.service.UpdateRole(c.Context(), id, req.Role)
	if err != nil {
		return err
	}

	return response.Success(c, user)
}

// BanUser godoc
// @Summary Ban a user
// @Description Soft delete a user (admin only)
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Success 204
// @Failure 400 {object} response.Response
// @Failure 401 {object} response.Response
// @Failure 403 {object} response.Response
// @Failure 404 {object} response.Response
// @Router /admin/users/{id}/ban [post]
func (h *AdminHandler) BanUser(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	if err := h.service.BanUser(c.Context(), id); err != nil {
		return err
	}

	return response.NoContent(c)
}

// UnbanUser godoc
// @Summary Unban a user
// @Description Restore a soft-deleted user (admin only)
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Success 200 {object} response.Response{data=dto.UserResponse}
// @Failure 400 {object} response.Response
// @Failure 401 {object} response.Response
// @Failure 403 {object} response.Response
// @Failure 404 {object} response.Response
// @Router /admin/users/{id}/unban [post]
func (h *AdminHandler) UnbanUser(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	user, err := h.service.UnbanUser(c.Context(), id)
	if err != nil {
		return err
	}

	return response.Success(c, user)
}

// ListFiles godoc
// @Summary List all files (admin)
// @Description Get a paginated list of all files
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param page query int false "Page number" default(1)
// @Param per_page query int false "Items per page" default(10)
// @Success 200 {object} response.Response{data=[]dto.FileResponse,meta=response.Meta}
// @Failure 401 {object} response.Response
// @Failure 403 {object} response.Response
// @Router /admin/files [get]
func (h *AdminHandler) ListFiles(c fiber.Ctx) error {
	page, perPage, err := paginationQuery(c)
	if err != nil {
		return err
	}

	files, total, err := h.service.ListFiles(c.Context(), page, perPage)
	if err != nil {
		return err
	}

	return response.SuccessWithMeta(c, files, response.NewMeta(page, perPage, total))
}
