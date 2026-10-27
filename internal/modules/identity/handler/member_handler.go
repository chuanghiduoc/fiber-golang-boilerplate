package handler

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// MemberAdminService is the identity member-administration surface. The
// concrete *service.adminUserService implements it through
// service.AdminUserService.
type MemberAdminService interface {
	ListMembers(ctx context.Context, limit int, startingAfter, status, role string) ([]service.Member, bool, error)
	GetMember(ctx context.Context, id uuid.UUID) (*service.Member, error)
	UpdateMember(ctx context.Context, id uuid.UUID, update service.MemberUpdate) (*service.Member, error)
	GetUserStats(ctx context.Context) (service.UserStats, error)
}

// MemberHandler serves tenant membership administration and statistics.
type MemberHandler struct {
	members MemberAdminService
	users   service.UserService
	roles   RoleLookup
}

func NewMemberHandler(members MemberAdminService, users service.UserService, roles RoleLookup) *MemberHandler {
	return &MemberHandler{members: members, users: users, roles: roles}
}

type listMembersInput struct {
	Limit         int    `query:"limit" minimum:"1" maximum:"100" default:"20"`
	StartingAfter string `query:"startingAfter"`
	Status        string `query:"status" enum:"active,suspended,removed"`
	Role          string `query:"role" maxLength:"50"`
}

type memberInput struct {
	MemberID string `path:"memberId" format:"uuid"`
}

type updateMemberInput struct {
	MemberID string `path:"memberId" format:"uuid"`
	IfMatch  string `header:"If-Match"`
	RawBody  []byte `rawBody:"application/merge-patch+json" contentType:"application/merge-patch+json" required:"true"`
}

type statsInput struct{}

// List returns the request tenant's memberships in every state.
func (h *MemberHandler) List(ctx context.Context, input *listMembersInput) (*httpapi.ListOutput[dto.UserResponse], error) {
	members, hasMore, err := h.members.ListMembers(ctx, paginationLimit(input.Limit), input.StartingAfter, input.Status, input.Role)
	if err != nil {
		return nil, err
	}
	out := toUserResponses(members)
	return &httpapi.ListOutput[dto.UserResponse]{
		Body: pagination.NewList("/api/v1/members", out, hasMore,
			pagination.NextCursor(out, func(u dto.UserResponse) (time.Time, uuid.UUID) { return u.JoinedAt, u.ID })),
	}, nil
}

// Get returns one member of the request tenant.
func (h *MemberHandler) Get(ctx context.Context, input *memberInput) (*httpapi.ETagOutput[dto.UserResponse], error) {
	id, err := parseUUID(input.MemberID, "memberId")
	if err != nil {
		return nil, err
	}
	member, err := h.members.GetMember(ctx, id)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.UserResponse]{
		ETag: etagHeader(member.UpdatedAt()),
		Body: toUserResponse(member),
	}, nil
}

// Update applies an RFC 7396 merge patch to a member's roles and status.
func (h *MemberHandler) Update(ctx context.Context, input *updateMemberInput) (*httpapi.ETagOutput[dto.UserResponse], error) {
	id, err := parseUUID(input.MemberID, "memberId")
	if err != nil {
		return nil, err
	}
	current, err := h.members.GetMember(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkETag(input.IfMatch, current.UpdatedAt()); err != nil {
		return nil, err
	}
	var req dto.MemberPatch
	if err := decodeMergePatch(input.RawBody, &req); err != nil {
		return nil, err
	}
	update := service.MemberUpdate{}
	if input.IfMatch != "" {
		update.ExpectedUpdatedAt = current.UpdatedAt()
	}
	if req.RoleIDs.Present {
		if req.RoleIDs.Null || len(req.RoleIDs.Value) == 0 {
			return nil, unprocessable("roleIds cannot be cleared")
		}
		keys, err := h.roleKeys(ctx, req.RoleIDs.Value)
		if err != nil {
			return nil, err
		}
		update.RoleKeys = keys
	}
	if req.Status.Present {
		if req.Status.Null {
			return nil, unprocessable("status cannot be cleared")
		}
		status := req.Status.Value
		if status != "active" && status != "suspended" && status != "removed" {
			return nil, unprocessable("status must be active, suspended, or removed")
		}
		update.Status = &status
	}
	updated, err := h.members.UpdateMember(ctx, id, update)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.UserResponse]{
		ETag: etagHeader(updated.UpdatedAt()),
		Body: toUserResponse(updated),
	}, nil
}

// Delete removes a member's membership from the request tenant.
func (h *MemberHandler) Delete(ctx context.Context, input *memberInput) (*httpapi.Empty, error) {
	id, err := parseUUID(input.MemberID, "memberId")
	if err != nil {
		return nil, err
	}
	if err := h.users.RemoveMember(ctx, id); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

// Stats returns the tenant's member counts.
func (h *MemberHandler) Stats(ctx context.Context, _ *statsInput) (*httpapi.Output[dto.StatsResponse], error) {
	stats, err := h.members.GetUserStats(ctx)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.StatsResponse]{Body: dto.StatsResponse{
		ActiveUsers:  stats.ActiveUsers,
		DeletedUsers: stats.DeletedUsers,
	}}, nil
}

// roleKeys resolves role ids to their keys, preserving request order.
func (h *MemberHandler) roleKeys(ctx context.Context, ids []uuid.UUID) ([]string, error) {
	if h.roles == nil {
		return nil, apperror.Internal(nil, "role lookup is not configured")
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		role, err := h.roles.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		keys = append(keys, role.Key)
	}
	return keys, nil
}
