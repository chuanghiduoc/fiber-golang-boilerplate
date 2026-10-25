package handler

import (
	"fmt"

	filescontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/files"
	identitycontracts "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts/identity"
	admindto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/dto"
	adminsvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/admin/service"
)

func toUserResponse(user identitycontracts.User) admindto.UserResponse {
	return admindto.UserResponse{
		ID:               user.ID,
		Email:            user.Email,
		Name:             user.Name,
		Role:             user.Role,
		MembershipStatus: user.MembershipStatus,
		EmailVerified:    user.EmailVerified,
		HasPassword:      user.HasPassword,
		CreatedAt:        user.CreatedAt,
		UpdatedAt:        user.UpdatedAt,
		JoinedAt:         user.JoinedAt,
		DeletedAt:        user.DeletedAt,
		BannedAt:         user.BannedAt,
	}
}

func toUserResponses(users []identitycontracts.User) []admindto.UserResponse {
	out := make([]admindto.UserResponse, len(users))
	for i := range users {
		out[i] = toUserResponse(users[i])
	}
	return out
}

// toFileResponse maps a files contract file to the admin response. A live file
// carries its download URL; a soft-deleted one carries deletedAt instead.
func toFileResponse(file filescontracts.File) admindto.FileResponse {
	resp := admindto.FileResponse{
		ID:           file.ID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
		DeletedAt:    file.DeletedAt,
	}
	if file.DeletedAt == nil {
		resp.URL = fmt.Sprintf("/api/v1/admin/files/%d/download", file.ID)
	}
	return resp
}

func toFileResponses(files []filescontracts.File) []admindto.FileResponse {
	out := make([]admindto.FileResponse, len(files))
	for i := range files {
		out[i] = toFileResponse(files[i])
	}
	return out
}

func toStatsResponse(stats adminsvc.Stats) admindto.AdminStatsResponse {
	return admindto.AdminStatsResponse{
		ActiveUsers:   stats.ActiveUsers,
		DeletedUsers:  stats.DeletedUsers,
		TotalFiles:    stats.TotalFiles,
		TotalFileSize: stats.TotalFileSize,
	}
}
