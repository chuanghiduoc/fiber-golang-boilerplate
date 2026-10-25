package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/apikey"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/authz/authzhttp"
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
	member, err := h.service.GetByID(c.Context(), httpx.AuthUserID(c))
	if err != nil {
		return err
	}

	return response.Success(c, toUserResponse(member))
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

	member, err := h.service.GetByID(c.Context(), id)
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

	return response.Success(c, toUserResponse(member))
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

	resp := toUserResponses(users)
	return response.List(c, resp, hasMore, pagination.NextCursor(resp, func(u dto.UserResponse) (time.Time, int64) { return u.CreatedAt, u.ID }))
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

	member, err := h.service.Update(c.Context(), httpx.AuthUserID(c), service.UpdateProfileInput{Name: req.Name, Email: req.Email})
	if err != nil {
		return err
	}
	h.sendVerificationAfterEmailChange(c.Context(), member, req)

	return response.Success(c, toUserResponse(member))
}

// Update godoc
// @Summary Update user by ID
// @Description Update the caller's own global profile. Users are global identities shared across tenants, so another user's profile (name or email) can never be changed, not even by a tenant admin; admins manage memberships (role, ban, removal).
// @Tags Users
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "User ID (must be the caller)"
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

	if id != httpx.AuthUserID(c) {
		return apperror.NewCode(apperror.CodePermissionDenied, "a user's profile can only be changed from their own session")
	}

	var req dto.UpdateUserRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	if err := h.requirePasswordForEmailChange(c, id, req); err != nil {
		return err
	}

	member, err := h.service.Update(c.Context(), id, service.UpdateProfileInput{Name: req.Name, Email: req.Email})
	if err != nil {
		return err
	}
	h.sendVerificationAfterEmailChange(c.Context(), member, req)

	return response.Success(c, toUserResponse(member))
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
	// identity change and must not demand re-authentication. Compare with the
	// service's normalization: any looser folding would let a real change
	// skip the password check.
	if service.NormalizeEmail(*req.Email) == service.NormalizeEmail(current.User.Email) {
		return nil
	}
	if !current.User.HasPassword {
		return apperror.NewBadRequest("email changes are not available for passwordless accounts")
	}
	if req.CurrentPassword == nil || *req.CurrentPassword == "" {
		return apperror.NewBadRequest("currentPassword is required to change email")
	}
	return h.service.VerifyPassword(withClientIP(c), userID, *req.CurrentPassword)
}

// authenticatedWithAPIKey reports whether apikeyhttp.Authenticate authenticated
// the request. Either marker is enough to deny.
func authenticatedWithAPIKey(c fiber.Ctx) bool {
	if fiber.Locals[string](c, apikey.LocalAuthMethod) == apikey.AuthMethodAPIKey {
		return true
	}
	subject, ok := authzhttp.SubjectFromFiber(c)
	return ok && subject.Attribute(authz.AttributeAuthMethod) == apikey.AuthMethodAPIKey
}

// sendVerificationAfterEmailChange invalidates tokens sent to the old address
// and sends a fresh verification link to the new one.
func (h *UserHandler) sendVerificationAfterEmailChange(ctx context.Context, member *service.Member, req dto.UpdateUserRequest) {
	if req.Email == nil || member.User.EmailVerified() || h.emailVerifSvc == nil {
		return
	}
	if err := h.emailVerifSvc.Invalidate(ctx, member.User.ID); err != nil {
		return
	}
	tenantSlug := tenant.SlugFromContext(ctx)
	async.Go(func() {
		sendCtx := tenant.WithIdentity(context.Background(), member.Membership.TenantID, tenantSlug)
		if err := h.emailVerifSvc.SendVerification(sendCtx, member.User.ID, member.User.Email); err != nil {
			slog.Error("post-commit verification email failed", slog.Int64("user_id", member.User.ID), slog.Any("error", err))
		}
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

	if err := h.service.ChangePassword(withClientIP(c), httpx.AuthUserID(c), service.ChangePasswordInput{
		CurrentPassword: req.CurrentPassword,
		NewPassword:     req.NewPassword,
	}); err != nil {
		return err
	}

	return response.Success(c, fiber.Map{"message": "password changed successfully"})
}

// Delete godoc
// @Summary Delete account or remove member
// @Description With the caller's own id: delete the caller's global account (every tenant; JWT only, API keys are refused; refused while the caller is the last active owner of any tenant). With another id: remove that user's membership from the request tenant (requires admin:users:delete); the global account and other tenants are untouched.
// @Tags Users
// @Security BearerAuth
// @Param id path int true "User ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "user.last_admin"
// @Router /users/{id} [delete]
func (h *UserHandler) Delete(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}

	if id == httpx.AuthUserID(c) {
		// Deleting the global account ends the user's access to every tenant,
		// which an API key scoped to one tenant must not be able to do.
		if authenticatedWithAPIKey(c) {
			return apperror.NewCode(apperror.CodePermissionDenied, "API keys cannot delete an account")
		}
		if err := h.service.DeleteAccount(c.Context(), id); err != nil {
			return err
		}
		return response.NoContent(c)
	}

	if !httpx.Can(c, authz.PermissionAdminUsersDelete) {
		return apperror.NewForbidden("you can only delete your own profile")
	}
	if err := h.service.RemoveMember(c.Context(), id); err != nil {
		return err
	}
	return response.NoContent(c)
}
