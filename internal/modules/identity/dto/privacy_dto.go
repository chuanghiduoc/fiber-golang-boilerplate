package dto

import (
	"time"

	"github.com/google/uuid"
)

// ErasureRequest re-authenticates the caller before a self-service erasure.
// CurrentPassword is required for an account with a local password; a
// passwordless (OAuth-only) account instead proves possession of a recent
// interactive session, so the field may be omitted there.
type ErasureRequest struct {
	CurrentPassword string `json:"currentPassword,omitempty" maxLength:"128"`
}

// DataExportResponse is one export request. The storage path is never exposed.
type DataExportResponse struct {
	ID         uuid.UUID  `json:"id"`
	TenantID   uuid.UUID  `json:"tenantId"`
	UserID     uuid.UUID  `json:"userId"`
	Status     string     `json:"status" enum:"queued,running,ready,failed,expired"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	ContentURL string     `json:"contentUrl,omitempty"`
}

// ErasureRequestResponse is the status resource for an erasure request.
type ErasureRequestResponse struct {
	ID          uuid.UUID  `json:"id"`
	TenantID    uuid.UUID  `json:"tenantId"`
	UserID      uuid.UUID  `json:"userId"`
	Scope       string     `json:"scope" enum:"self,member"`
	Status      string     `json:"status" enum:"queued,processing,completed"`
	RequestedAt time.Time  `json:"requestedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}
