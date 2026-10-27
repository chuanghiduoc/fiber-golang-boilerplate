package handler

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/membership"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// InvitationService is the invitation surface the handlers use.
// *membership.InvitationService implements it.
type InvitationService interface {
	Invite(ctx context.Context, in membership.InviteInput) (*core.Invitation, error)
	List(ctx context.Context, limit int, startingAfter string) ([]core.Invitation, bool, error)
	Get(ctx context.Context, id uuid.UUID) (*core.Invitation, error)
	Revoke(ctx context.Context, id uuid.UUID) error
	Accept(ctx context.Context, userID uuid.UUID, token string) (*core.Member, error)
	SignUp(ctx context.Context, in membership.InvitationSignUp) (*core.Member, error)
}

// RoleLookup resolves a role id to its definition for invitation creation.
type RoleLookup interface {
	Get(ctx context.Context, id uuid.UUID) (*core.Role, error)
}

// InvitationHandler serves tenant invitations.
type InvitationHandler struct {
	invitations InvitationService
	roles       RoleLookup
}

func NewInvitationHandler(invitations InvitationService, roles ...RoleLookup) *InvitationHandler {
	h := &InvitationHandler{invitations: invitations}
	if len(roles) > 0 {
		h.roles = roles[0]
	}
	return h
}

func toInvitationResponse(inv *core.Invitation) dto.InvitationResponse {
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

type createInvitationInput struct {
	Body dto.InviteRequest
}

type listInvitationsInput struct {
	Limit         int    `query:"limit" minimum:"1" maximum:"100" default:"20"`
	StartingAfter string `query:"startingAfter"`
}

type invitationInput struct {
	InvitationID string `path:"invitationId" format:"uuid"`
}

// Create invites an address into the request tenant.
func (h *InvitationHandler) Create(ctx context.Context, input *createInvitationInput) (*httpapi.Created[dto.InvitationResponse], error) {
	role := core.RoleMember
	if len(input.Body.RoleIDs) > 0 {
		if len(input.Body.RoleIDs) > 1 {
			return nil, unprocessable("an invitation grants exactly one role")
		}
		if h.roles == nil {
			return nil, apperror.Internal(nil, "role lookup is not configured")
		}
		found, err := h.roles.Get(ctx, input.Body.RoleIDs[0])
		if err != nil {
			return nil, err
		}
		role = found.Key
	}
	inv, err := h.invitations.Invite(ctx, membership.InviteInput{
		InviterID: actorID(ctx),
		Email:     input.Body.Email,
		Role:      role,
	})
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated("/api/v1/invitations/"+inv.ID.String(), toInvitationResponse(inv)), nil
}

// List returns pending invitations of the request tenant.
func (h *InvitationHandler) List(ctx context.Context, input *listInvitationsInput) (*httpapi.ListOutput[dto.InvitationResponse], error) {
	invitations, hasMore, err := h.invitations.List(ctx, paginationLimit(input.Limit), input.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := make([]dto.InvitationResponse, len(invitations))
	for i := range invitations {
		out[i] = toInvitationResponse(&invitations[i])
	}
	return &httpapi.ListOutput[dto.InvitationResponse]{
		Body: pagination.NewList("/api/v1/invitations", out, hasMore,
			pagination.NextCursor(out, func(inv dto.InvitationResponse) (time.Time, uuid.UUID) { return inv.CreatedAt, inv.ID })),
	}, nil
}

// Get returns one invitation of the request tenant.
func (h *InvitationHandler) Get(ctx context.Context, input *invitationInput) (*httpapi.Output[dto.InvitationResponse], error) {
	id, err := parseUUID(input.InvitationID, "invitationId")
	if err != nil {
		return nil, err
	}
	inv, err := h.invitations.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.InvitationResponse]{Body: toInvitationResponse(inv)}, nil
}

// Revoke deletes a pending invitation.
func (h *InvitationHandler) Revoke(ctx context.Context, input *invitationInput) (*httpapi.Empty, error) {
	id, err := parseUUID(input.InvitationID, "invitationId")
	if err != nil {
		return nil, err
	}
	if err := h.invitations.Revoke(ctx, id); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// Accept joins the invitation's tenant as the signed-in user.
func (h *InvitationHandler) Accept(ctx context.Context, input *acceptInvitationInput) (*httpapi.Output[dto.UserResponse], error) {
	member, err := h.invitations.Accept(ctx, actorID(ctx), input.Body.Token)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.UserResponse]{Body: toUserResponse(member)}, nil
}

// SignUp creates an account from an invitation.
func (h *InvitationHandler) SignUp(ctx context.Context, input *signupInvitationInput) (*httpapi.Created[dto.UserResponse], error) {
	member, err := h.invitations.SignUp(ctx, membership.InvitationSignUp{
		Token:    input.Body.Token,
		Name:     input.Body.Name,
		Password: input.Body.Password,
	})
	if err != nil {
		return nil, err
	}
	auditActor(ctx, member.User.ID)
	return httpapi.NewCreated("/api/v1/me/profile", toUserResponse(member)), nil
}
