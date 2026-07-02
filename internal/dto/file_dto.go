package dto

import "time"

type FileResponse struct {
	ID           int64     `json:"id"`
	OriginalName string    `json:"originalName"`
	MimeType     string    `json:"mimeType"`
	Size         int64     `json:"size"`
	URL          string    `json:"url"`
	CreatedAt    time.Time `json:"createdAt"`
}
