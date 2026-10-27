package handler

import (
	"fmt"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/dto"
	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
)

// toFileResponse maps a service file to the user-facing response. A live file
// carries its download URL; a soft-deleted one carries deletedAt instead.
func toFileResponse(file *filesvc.File) dto.FileResponse {
	resp := dto.FileResponse{
		ID:           file.ID,
		TenantID:     file.TenantID,
		UserID:       file.UserID,
		OriginalName: file.OriginalName,
		MimeType:     file.MimeType,
		Size:         file.Size,
		CreatedAt:    file.CreatedAt,
		DeletedAt:    file.DeletedAt,
	}
	if file.DeletedAt == nil {
		resp.URL = fmt.Sprintf("/api/v1/files/%s/content", file.ID)
	}
	return resp
}

func toFileResponses(files []filesvc.File) []dto.FileResponse {
	out := make([]dto.FileResponse, len(files))
	for i := range files {
		out[i] = toFileResponse(&files[i])
	}
	return out
}
