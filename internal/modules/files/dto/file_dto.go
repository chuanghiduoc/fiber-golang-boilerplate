package dto

import "time"

type FileResponse struct {
	ID           int64      `json:"id"`
	TenantID     int64      `json:"tenantId"`
	UserID       int64      `json:"userId,omitempty"`
	OriginalName string     `json:"originalName"`
	MimeType     string     `json:"mimeType"`
	Size         int64      `json:"size"`
	URL          string     `json:"url"`
	CreatedAt    time.Time  `json:"createdAt"`
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
}
