package handler

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
)

// toUserResponse maps a member of the request tenant to the API shape. Role,
// ban, and join time come from the membership; identity fields from the
// global user. DeletedAt is the membership removal or the account deletion.
func toUserResponse(member *service.Member) dto.UserResponse {
	user, membership := member.User, member.Membership
	resp := dto.UserResponse{
		ID:               user.ID,
		Email:            user.Email,
		Name:             user.Name,
		Role:             membership.Role,
		Roles:            membership.Roles,
		MembershipStatus: string(membership.Status),
		EmailVerified:    user.EmailVerified(),
		HasPassword:      user.HasPassword,
		CreatedAt:        user.CreatedAt,
		UpdatedAt:        user.UpdatedAt,
		JoinedAt:         membership.CreatedAt,
		BannedAt:         membership.BannedAt,
		TokenVersion:     user.TokenVersion,
		TenantID:         membership.TenantID,
	}
	if membership.UpdatedAt.After(resp.UpdatedAt) {
		resp.UpdatedAt = membership.UpdatedAt
	}
	switch {
	case membership.RemovedAt != nil:
		resp.DeletedAt = membership.RemovedAt
	case user.DeletedAt != nil:
		resp.DeletedAt = user.DeletedAt
	}
	return resp
}

func toUserResponses(members []service.Member) []dto.UserResponse {
	out := make([]dto.UserResponse, len(members))
	for i := range members {
		out[i] = toUserResponse(&members[i])
	}
	return out
}
