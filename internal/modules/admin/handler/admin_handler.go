package handler

import (
	"time"

	"github.com/gofiber/fiber/v3"

	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	adminsvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// Handler serves the back-office HTTP surface. It owns only admin DTOs and
// delegates cross-module work to the admin service contracts.
type Handler struct {
	service adminsvc.Service
}

func NewHandler(svc adminsvc.Service) *Handler {
	return &Handler{service: svc}
}

// GetStats godoc
// @Summary Get tenant statistics
// @Description Get statistics for the caller's tenant only (admin only): activeUsers counts active memberships of active users, deletedUsers every other membership
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Success 200 {object} admindto.AdminStatsResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/stats [get]
func (h *Handler) GetStats(c fiber.Ctx) error {
	stats, err := h.service.GetStats(c.Context())
	if err != nil {
		return err
	}

	return response.Success(c, toStatsResponse(stats))
}

// ListUsers godoc
// @Summary List tenant users (admin)
// @Description Get a paginated list of the members of the caller's tenant in every membership state (active, suspended, removed), ordered by join time
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]admindto.UserResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/users [get]
func (h *Handler) ListUsers(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}

	page, err := h.service.ListUsers(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}

	resp := toUserResponses(page.Items)
	return response.List(c, resp, page.HasMore, pagination.NextCursor(resp, func(u admindto.UserResponse) (time.Time, int64) { return u.JoinedAt, u.ID }))
}

// UpdateRole godoc
// @Summary Update user role
// @Description Change a member's role in the caller's tenant (admin only); the member's sessions in this tenant end
// @Tags Admin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Param request body admindto.UpdateRoleRequest true "Role update request"
// @Success 200 {object} admindto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/users/{id}/role [put]
func (h *Handler) UpdateRole(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if id == httpx.AuthUserID(c) {
		return apperror.NewBadRequest("you cannot change your own role")
	}

	var req admindto.UpdateRoleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.service.UpdateRole(c.Context(), id, req.Role)
	if err != nil {
		return err
	}

	return response.Success(c, toUserResponse(user))
}

// BanUser godoc
// @Summary Ban a user
// @Description Ban a member of the caller's tenant (membership suspended; sessions and API keys in this tenant end; the global account and other tenants are untouched)
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
func (h *Handler) BanUser(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if id == httpx.AuthUserID(c) {
		return apperror.NewBadRequest("you cannot ban your own account")
	}

	if err := h.service.BanUser(c.Context(), id); err != nil {
		return err
	}

	return response.NoContent(c)
}

// UnbanUser godoc
// @Summary Unban a user
// @Description Lift a member's ban in the caller's tenant (API keys issued before the ban stay revoked)
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Success 200 {object} admindto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/users/{id}/unban [post]
func (h *Handler) UnbanUser(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}

	user, err := h.service.UnbanUser(c.Context(), id)
	if err != nil {
		return err
	}

	return response.Success(c, toUserResponse(user))
}

// ListFiles godoc
// @Summary List tenant files (admin)
// @Description Get a paginated list of files in the caller's tenant
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]admindto.FileResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/files [get]
func (h *Handler) ListFiles(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}

	page, err := h.service.ListFiles(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}

	resp := toFileResponses(page.Items)
	return response.List(c, resp, page.HasMore, pagination.NextCursor(resp, func(f admindto.FileResponse) (time.Time, int64) { return f.CreatedAt, f.ID }))
}

// DownloadFile godoc
// @Summary Download a tenant file (admin)
// @Description Download an active file in the caller's tenant by ID
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
func (h *Handler) DownloadFile(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}

	download, err := h.service.Download(c.Context(), id)
	if err != nil {
		return err
	}

	file := download.File
	c.Set("Content-Type", file.MimeType)
	c.Set("Content-Disposition", httpx.ContentDispositionAttachment(file.OriginalName))
	return c.SendStream(download.Reader, int(file.Size))
}

// DeleteFile godoc
// @Summary Soft-delete a tenant file (admin)
// @Description Soft-delete a file in the caller's tenant by ID regardless of owner
// @Tags Admin
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/files/{id} [delete]
func (h *Handler) DeleteFile(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.service.DeleteFile(c.Context(), id); err != nil {
		return err
	}
	return response.NoContent(c)
}

// RestoreFile godoc
// @Summary Restore a tenant file (admin)
// @Description Restore any soft-deleted file in the caller's tenant by ID
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param id path int true "File ID"
// @Success 200 {object} admindto.FileResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/files/{id}/restore [post]
func (h *Handler) RestoreFile(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}

	file, err := h.service.RestoreFile(c.Context(), id)
	if err != nil {
		return err
	}
	return response.Success(c, toFileResponse(file))
}
