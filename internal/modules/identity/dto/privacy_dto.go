package dto

import "time"

// ErasureRequest re-authenticates the caller before a self-service erasure.
type ErasureRequest struct {
	CurrentPassword string `json:"currentPassword" validate:"required"`
}

// DataExportResponse is one export request. The storage path is never exposed.
type DataExportResponse struct {
	ID          int64      `json:"id"`
	TenantID    int64      `json:"tenantId"`
	UserID      int64      `json:"userId"`
	Status      string     `json:"status" enums:"queued,running,ready,failed,expired"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	DownloadURL string     `json:"downloadUrl,omitempty"`
}

// ErasureAccepted reports that erasure work was queued.
type ErasureAccepted struct {
	Status string `json:"status"`
}
