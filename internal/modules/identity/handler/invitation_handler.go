package handler

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// InvitationService is the invitation surface the handlers use.
// *service.InvitationService implements it.
type InvitationService interface {
	Invite(ctx context.Context, in service.InviteInput) (*service.Invitation, error)
	List(ctx context.Context, limit int, startingAfter string) ([]service.Invitation, bool, error)
	Revoke(ctx context.Context, id int64) error
	Accept(ctx context.Context, userID int64, token string) (*service.Member, error)
	SignUp(ctx context.Context, in service.InvitationSignUp) (*service.Member, error)
}

// InvitationHandler serves tenant invitations: admins invite, list, and
// revoke; invitees accept (signed in) or sign up from the emailed token.
type InvitationHandler struct {
	invitations InvitationService
}

func NewInvitationHandler(invitations InvitationService) *InvitationHandler {
	return &InvitationHandler{invitations: invitations}
}

func toInvitationResponse(inv *service.Invitation) dto.InvitationResponse {
	return dto.InvitationResponse{
		ID:         inv.ID,
		TenantID:   inv.TenantID,
		Email:      inv.Email,
		Role:       inv.Role,
		InvitedBy:  inv.InvitedBy,
		ExpiresAt:  inv.ExpiresAt,
		AcceptedAt: inv.AcceptedAt,
		CreatedAt:  inv.CreatedAt,
	}
}

// Create godoc
// @Summary Invite a member
// @Description Invite an email address into the request tenant (role defaults to user). Re-inviting a pending address replaces its token, role, and expiry. The link is emailed; the token is never returned.
// @Tags Invitations
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param Idempotency-Key header string false "Idempotency key"
// @Param request body dto.InviteRequest true "Invitation"
// @Success 201 {object} dto.InvitationResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "membership.exists"
// @Failure 422 {object} apperror.ProblemDetails
// @Router /invitations [post]
func (h *InvitationHandler) Create(c fiber.Ctx) error {
	var req dto.InviteRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	role := req.Role
	if role == "" {
		role = service.RoleMember
	}
	inv, err := h.invitations.Invite(c.Context(), service.InviteInput{
		InviterID: httpx.AuthUserID(c),
		Email:     req.Email,
		Role:      role,
	})
	if err != nil {
		return err
	}
	return response.Created(c, toInvitationResponse(inv))
}

// List godoc
// @Summary List pending invitations
// @Description Pending invitations of the request tenant, newest first.
// @Tags Invitations
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.InvitationResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /invitations [get]
func (h *InvitationHandler) List(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	invitations, hasMore, err := h.invitations.List(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}
	out := make([]dto.InvitationResponse, len(invitations))
	for i := range invitations {
		out[i] = toInvitationResponse(&invitations[i])
	}
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(inv dto.InvitationResponse) (time.Time, int64) { return inv.CreatedAt, inv.ID }))
}

// Revoke godoc
// @Summary Revoke an invitation
// @Description Delete a pending invitation of the request tenant; its link stops working.
// @Tags Invitations
// @Security BearerAuth
// @Param id path int true "Invitation ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /invitations/{id} [delete]
func (h *InvitationHandler) Revoke(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.invitations.Revoke(c.Context(), id); err != nil {
		return err
	}
	return response.NoContent(c)
}

// Accept godoc
// @Summary Accept an invitation
// @Description Join the invitation's tenant as the signed-in user. The token is checked by possession (the caller is signed in to another tenant); the invitation must be addressed to the caller's current email. Afterwards sign in with X-Tenant-Slug of the new tenant.
// @Tags Invitations
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.AcceptInvitationRequest true "Invitation token"
// @Success 200 {object} dto.UserResponse "the membership in the invitation's tenant"
// @Failure 400 {object} apperror.ProblemDetails "invitation.invalid"
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "membership.exists"
// @Failure 422 {object} apperror.ProblemDetails
// @Router /auth/invitations/accept [post]
func (h *InvitationHandler) Accept(c fiber.Ctx) error {
	var req dto.AcceptInvitationRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	member, err := h.invitations.Accept(c.Context(), httpx.AuthUserID(c), req.Token)
	if err != nil {
		return err
	}
	return response.Success(c, toUserResponse(member))
}

// SignUp godoc
// @Summary Sign up from an invitation
// @Description Create an account for the invited address and join the invitation's tenant (send X-Tenant-Slug from the link's tenant parameter). The email is verified by the token. An address that already has an account gets 409 user.email_taken: sign in and accept instead. Not gated by user_registration.
// @Tags Auth
// @Accept json
// @Produce json
// @Param request body dto.InvitationSignUpRequest true "Invitation sign-up"
// @Success 201 {object} dto.UserResponse
// @Failure 400 {object} apperror.ProblemDetails "invitation.invalid"
// @Failure 409 {object} apperror.ProblemDetails "user.email_taken"
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 429 {object} apperror.ProblemDetails
// @Router /auth/invitations/signup [post]
func (h *InvitationHandler) SignUp(c fiber.Ctx) error {
	var req dto.InvitationSignUpRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	member, err := h.invitations.SignUp(c.Context(), service.InvitationSignUp{
		Token:    req.Token,
		Name:     req.Name,
		Password: req.Password,
	})
	if err != nil {
		return err
	}
	// Attribute the audit/log line to the new account, as Register does.
	fiber.Locals(c, "user_id", member.User.ID)
	return response.Created(c, toUserResponse(member))
}
