package handler

import (
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
)

// toUserResponse maps a member of the request tenant to the API shape. Role,
// ban, and join time come from the membership; identity fields from the
// global user. DeletedAt is the membership removal or the account deletion.
func toUserResponse(member *core.Member) dto.UserResponse {
	user, membership := member.User, member.Membership
	return dto.UserResponse{
		ID:               user.ID,
		Email:            user.Email,
		Name:             user.Name,
		Role:             membership.Role,
		Roles:            membership.Roles,
		MembershipStatus: string(membership.Status),
		EmailVerified:    user.EmailVerified(),
		HasPassword:      user.HasPassword,
		CreatedAt:        user.CreatedAt,
		UpdatedAt:        member.UpdatedAt(),
		JoinedAt:         membership.CreatedAt,
		DeletedAt:        member.DeletedAt(),
		BannedAt:         membership.BannedAt,
		TokenVersion:     user.TokenVersion,
		TenantID:         membership.TenantID,
	}
}

func toUserResponses(members []core.Member) []dto.UserResponse {
	out := make([]dto.UserResponse, len(members))
	for i := range members {
		out[i] = toUserResponse(&members[i])
	}
	return out
}
