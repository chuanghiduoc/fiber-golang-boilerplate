package handler

import (
	"context"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/async"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

type UserHandler struct {
	service       service.UserService
	emailVerifSvc service.EmailVerificationService
}

func NewUserHandler(svc service.UserService, emailVerif ...service.EmailVerificationService) *UserHandler {
	h := &UserHandler{service: svc}
	if len(emailVerif) > 0 {
		h.emailVerifSvc = emailVerif[0]
	}
	return h
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
	user, err := h.service.GetByID(c.Context(), httpx.AuthUserID(c))
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
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}

	user, err := h.service.GetByID(c.Context(), id)
	if err != nil {
		return err
	}

	// Only the account owner or an admin may read a profile (it contains PII
	// such as the email address and role).
	if id != httpx.AuthUserID(c) && !httpx.Can(c, authz.PermissionAdminUsersRead) {
		// 404 rather than 403 avoids confirming that an otherwise private
		// account ID exists.
		return apperror.NewNotFound("user not found")
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
// @Failure 403 {object} apperror.ProblemDetails
// @Router /users [get]
func (h *UserHandler) List(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}

	users, hasMore, err := h.service.List(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}

	return response.List(c, users, hasMore, pagination.NextCursor(users, func(u dto.UserResponse) (time.Time, int64) { return u.CreatedAt, u.ID }))
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
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.requirePasswordForEmailChange(c, httpx.AuthUserID(c), req); err != nil {
		return err
	}

	user, err := h.service.Update(c.Context(), httpx.AuthUserID(c), req)
	if err != nil {
		return err
	}
	h.sendVerificationAfterEmailChange(c.Context(), user, req)

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
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}

	if id != httpx.AuthUserID(c) && !httpx.Can(c, authz.PermissionAdminUsersWrite) {
		return apperror.NewForbidden("you can only update your own profile")
	}

	var req dto.UpdateUserRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if id == httpx.AuthUserID(c) {
		if err := h.requirePasswordForEmailChange(c, id, req); err != nil {
			return err
		}
	}

	user, err := h.service.Update(c.Context(), id, req)
	if err != nil {
		return err
	}
	h.sendVerificationAfterEmailChange(c.Context(), user, req)

	return response.Success(c, user)
}

// requirePasswordForEmailChange forces a re-authentication step before a user
// can move their account to a different address. Without it, a stolen access
// token is enough to redirect a password-reset email to the attacker.
func (h *UserHandler) requirePasswordForEmailChange(c fiber.Ctx, userID int64, req dto.UpdateUserRequest) error {
	if req.Email == nil {
		return nil
	}

	current, err := h.service.GetByID(c.Context(), userID)
	if err != nil {
		return err
	}
	// A full-resource PUT often echoes the existing email; that is not an
	// identity change and must not demand re-authentication.
	if strings.EqualFold(strings.TrimSpace(*req.Email), current.Email) {
		return nil
	}
	if !current.HasPassword {
		return apperror.NewBadRequest("email changes are not available for passwordless accounts")
	}
	if req.CurrentPassword == nil || *req.CurrentPassword == "" {
		return apperror.NewBadRequest("currentPassword is required to change email")
	}
	return h.service.VerifyPassword(c.Context(), userID, *req.CurrentPassword)
}

// sendVerificationAfterEmailChange invalidates tokens sent to the old address
// and sends a fresh verification link to the new one.
func (h *UserHandler) sendVerificationAfterEmailChange(ctx context.Context, user *dto.UserResponse, req dto.UpdateUserRequest) {
	if req.Email == nil || user.EmailVerified || h.emailVerifSvc == nil {
		return
	}
	if err := h.emailVerifSvc.Invalidate(ctx, user.ID); err != nil {
		return
	}
	tenantSlug := tenant.SlugFromContext(ctx)
	async.Go(func() {
		sendCtx := tenant.WithIdentity(context.Background(), user.TenantID, tenantSlug)
		_ = h.emailVerifSvc.SendVerification(sendCtx, user.ID, user.Email)
	})
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
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	if err := h.service.ChangePassword(c.Context(), httpx.AuthUserID(c), req); err != nil {
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
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}

	if id != httpx.AuthUserID(c) && !httpx.Can(c, authz.PermissionAdminUsersDelete) {
		return apperror.NewForbidden("you can only delete your own profile")
	}

	if err := h.service.Delete(c.Context(), id); err != nil {
		return err
	}

	return response.NoContent(c)
}
