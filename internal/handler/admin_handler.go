package handler

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
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
// @Success 200 {object} dto.AdminStatsResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
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
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.UserResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/users [get]
func (h *AdminHandler) ListUsers(c fiber.Ctx) error {
	limit, startingAfter, err := cursorQuery(c)
	if err != nil {
		return err
	}

	users, hasMore, err := h.service.ListUsers(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}

	return response.List(c, users, hasMore, nextUserCursor(users))
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
// @Success 200 {object} dto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/users/{id}/role [put]
func (h *AdminHandler) UpdateRole(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}
	if id == authUserID(c) {
		return apperror.NewBadRequest("you cannot change your own role")
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
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/users/{id}/ban [post]
func (h *AdminHandler) BanUser(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}
	if id == authUserID(c) {
		return apperror.NewBadRequest("you cannot ban your own account")
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
// @Success 200 {object} dto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
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
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.FileResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/files [get]
func (h *AdminHandler) ListFiles(c fiber.Ctx) error {
	limit, startingAfter, err := cursorQuery(c)
	if err != nil {
		return err
	}

	files, hasMore, err := h.service.ListFiles(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}

	return response.List(c, files, hasMore, nextFileCursor(files))
}

// DownloadFile godoc
// @Summary Download any file (admin)
// @Description Download an active file by ID
// @Tags Admin
// @Produce octet-stream
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 200
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/files/{id}/download [get]
func (h *AdminHandler) DownloadFile(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	file, reader, err := h.service.Download(c.Context(), id)
	if err != nil {
		return err
	}

	c.Set("Content-Type", file.MimeType)
	c.Set("Content-Disposition", contentDispositionAttachment(file.OriginalName))
	return c.SendStream(reader, int(file.Size))
}

// DeleteFile godoc
// @Summary Soft-delete any file (admin)
// @Description Soft-delete a file by ID regardless of owner
// @Tags Admin
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/files/{id} [delete]
func (h *AdminHandler) DeleteFile(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}
	if err := h.service.DeleteFile(c.Context(), id); err != nil {
		return err
	}
	return response.NoContent(c)
}

// RestoreFile godoc
// @Summary Restore a soft-deleted file (admin)
// @Description Restore any file by ID
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 200 {object} dto.FileResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/files/{id}/restore [post]
func (h *AdminHandler) RestoreFile(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	file, err := h.service.RestoreFile(c.Context(), id)
	if err != nil {
		return err
	}
	return response.Success(c, file)
}
