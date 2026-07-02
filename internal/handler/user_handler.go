package handler

import (
	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

type UserHandler struct {
	service service.UserService
}

func NewUserHandler(svc service.UserService) *UserHandler {
	return &UserHandler{service: svc}
}

// GetMe godoc
// @Summary Get current user
// @Description Get the authenticated user's profile
// @Tags Users
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.UserResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Router /users/me [get]
func (h *UserHandler) GetMe(c fiber.Ctx) error {
	user, err := h.service.GetByID(c.Context(), authUserID(c))
	if err != nil {
		return err
	}

	return response.Success(c, user)
}

// GetByID godoc
// @Summary Get user by ID
// @Description Get a user by their ID
// @Tags Users
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Success 200 {object} dto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /users/{id} [get]
func (h *UserHandler) GetByID(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	user, err := h.service.GetByID(c.Context(), id)
	if err != nil {
		return err
	}

	return response.Success(c, user)
}

// List godoc
// @Summary List users
// @Description Get a paginated list of users
// @Tags Users
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.UserResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Router /users [get]
func (h *UserHandler) List(c fiber.Ctx) error {
	limit, startingAfter, err := cursorQuery(c)
	if err != nil {
		return err
	}

	users, hasMore, err := h.service.List(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}

	return response.List(c, users, hasMore)
}

// UpdateMe godoc
// @Summary Update current user
// @Description Update the authenticated user's profile
// @Tags Users
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.UpdateUserRequest true "Update request"
// @Success 200 {object} dto.UserResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /users/me [put]
func (h *UserHandler) UpdateMe(c fiber.Ctx) error {
	var req dto.UpdateUserRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.service.Update(c.Context(), authUserID(c), req)
	if err != nil {
		return err
	}

	return response.Success(c, user)
}

// Update godoc
// @Summary Update user by ID
// @Description Update a user's profile (admin or self)
// @Tags Users
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID"
// @Param request body dto.UpdateUserRequest true "Update request"
// @Success 200 {object} dto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /users/{id} [put]
func (h *UserHandler) Update(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	if id != authUserID(c) && authRole(c) != dto.RoleAdmin {
		return apperror.NewForbidden("you can only update your own profile")
	}

	var req dto.UpdateUserRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	user, err := h.service.Update(c.Context(), id, req)
	if err != nil {
		return err
	}

	return response.Success(c, user)
}

// ChangePassword godoc
// @Summary Change password
// @Description Change the authenticated user's password
// @Tags Users
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.ChangePasswordRequest true "Change password request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /users/me/password [put]
func (h *UserHandler) ChangePassword(c fiber.Ctx) error {
	var req dto.ChangePasswordRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.ChangePassword(c.Context(), authUserID(c), req); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "password changed successfully"})
}

// Delete godoc
// @Summary Delete user
// @Description Delete a user by ID
// @Tags Users
// @Security BearerAuth
// @Param id path int true "User ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /users/{id} [delete]
func (h *UserHandler) Delete(c fiber.Ctx) error {
	id, err := paramID(c, "id")
	if err != nil {
		return err
	}

	if id != authUserID(c) && authRole(c) != dto.RoleAdmin {
		return apperror.NewForbidden("you can only delete your own profile")
	}

	if err := h.service.Delete(c.Context(), id); err != nil {
		return err
	}

	return response.NoContent(c)
}
