package dto

import (
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// FileResponse is the wire representation of one file metadata row. The
// storage path is never exposed; a live file carries its content URL.
type FileResponse struct {
	ID           uuid.UUID  `json:"id" doc:"File ID."`
	TenantID     uuid.UUID  `json:"tenantId" doc:"Owning tenant ID."`
	UserID       uuid.UUID  `json:"userId,omitempty" doc:"Owning user ID."`
	OriginalName string     `json:"originalName" doc:"Client-supplied file name, sanitized."`
	MimeType     string     `json:"mimeType" doc:"Detected media type."`
	Size         int64      `json:"size" doc:"Size in bytes."`
	URL          string     `json:"url" doc:"Content URL; empty for a trashed file."`
	CreatedAt    time.Time  `json:"createdAt" doc:"Upload time."`
	DeletedAt    *time.Time `json:"deletedAt,omitempty" doc:"Soft-delete time, when trashed."`
}

// FileIDInput is the {fileId} path parameter shared by the item operations.
type FileIDInput struct {
	FileID string `path:"fileId" doc:"File ID (UUID)."`
}

// ListFilesInput is the collection query. The service narrows a caller without
// admin:files:read to their own rows.
type ListFilesInput struct {
	Limit          int    `query:"limit" default:"20" doc:"Page size (values above 100 are clamped)."`
	StartingAfter  string `query:"startingAfter" doc:"Cursor from the last item of the previous page."`
	IncludeDeleted bool   `query:"includeDeleted" doc:"Include soft-deleted files (trash). Tenant admins always see the whole tenant."`
}

// CreateFileInput is the multipart upload body.
type CreateFileInput struct {
	RawBody huma.MultipartFormFiles[CreateFileForm]
}

// CreateFileForm names the multipart file part.
type CreateFileForm struct {
	File huma.FormFile `form:"file" required:"false" contentType:"application/octet-stream" doc:"File to upload."`
}
