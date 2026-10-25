package handler

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// RoleHandler serves tenant role management and member role replacement.
type RoleHandler struct {
	roles service.RoleService
}

func NewRoleHandler(roles service.RoleService) *RoleHandler {
	return &RoleHandler{roles: roles}
}

// ListRoles godoc
// @Summary List roles
// @Description System roles plus the request tenant's roles, newest first
// @Tags Roles
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.RoleResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/roles [get]
func (h *RoleHandler) ListRoles(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	roles, hasMore, err := h.roles.List(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}
	out := make([]dto.RoleResponse, len(roles))
	for i := range roles {
		out[i] = toRoleResponse(&roles[i])
	}
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(r dto.RoleResponse) (time.Time, int64) {
		return r.CreatedAt, r.ID
	}))
}

// CreateRole godoc
// @Summary Create a tenant role
// @Description Create a tenant role with known tenant permissions
// @Tags Roles
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.CreateRoleRequest true "Role"
// @Success 201 {object} dto.RoleResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/roles [post]
func (h *RoleHandler) CreateRole(c fiber.Ctx) error {
	var req dto.CreateRoleRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	created, err := h.roles.Create(c.Context(), service.NewRole{
		Key:         req.Key,
		Name:        req.Name,
		Permissions: req.Permissions,
	})
	if err != nil {
		return err
	}
	return response.Created(c, toRoleResponse(created))
}

// UpdateRole godoc
// @Summary Update a tenant role
// @Description Change a tenant role's name and/or permission set
// @Tags Roles
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "Role ID"
// @Param request body dto.UpdateRoleDefinitionRequest true "Role update"
// @Success 200 {object} dto.RoleResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/roles/{id} [patch]
func (h *RoleHandler) UpdateRole(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	var req dto.UpdateRoleDefinitionRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	updated, err := h.roles.Update(c.Context(), id, service.RoleUpdate{
		Name:        req.Name,
		Permissions: req.Permissions,
	})
	if err != nil {
		return err
	}
	return response.Success(c, toRoleResponse(updated))
}

// DeleteRole godoc
// @Summary Delete a tenant role
// @Description Delete an unassigned tenant role
// @Tags Roles
// @Security BearerAuth
// @Param id path int true "Role ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/roles/{id} [delete]
func (h *RoleHandler) DeleteRole(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.roles.Delete(c.Context(), id); err != nil {
		return err
	}
	return response.NoContent(c)
}

// ReplaceMemberRoles godoc
// @Summary Replace a member's roles
// @Description Replace every role of a member in the request tenant; the member's sessions end
// @Tags Roles
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Param request body dto.ReplaceMemberRolesRequest true "Roles"
// @Success 200 {object} dto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/users/{id}/roles [put]
func (h *RoleHandler) ReplaceMemberRoles(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if id == httpx.AuthUserID(c) {
		return apperror.NewBadRequest("you cannot change your own roles")
	}
	var req dto.ReplaceMemberRolesRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	member, err := h.roles.ReplaceMemberRoles(c.Context(), id, req.Roles)
	if err != nil {
		return err
	}
	return response.Success(c, toUserResponse(member))
}

func toRoleResponse(role *service.Role) dto.RoleResponse {
	return dto.RoleResponse{
		ID:          role.ID,
		Key:         role.Key,
		Name:        role.Name,
		Scope:       role.Scope,
		System:      role.System(),
		Permissions: role.Permissions,
		CreatedAt:   role.CreatedAt,
		UpdatedAt:   role.UpdatedAt,
	}
}
