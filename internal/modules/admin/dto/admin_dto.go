// Package admindto contains the back-office HTTP request and response types.
// It keeps the admin wire contract independent of identity/files DTOs so those
// modules can evolve without changing the public admin API.
package admindto

import "time"

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

type UpdateRoleRequest struct {
	Role string `json:"role" validate:"required,oneof=user admin"`
}

type AdminStatsResponse struct {
	ActiveUsers   int64 `json:"activeUsers"`
	DeletedUsers  int64 `json:"deletedUsers"`
	TotalFiles    int64 `json:"totalFiles"`
	TotalFileSize int64 `json:"totalFileSize"`
}

// UserResponse is a member of the request tenant: identity fields from the
// global user, role/status/joinedAt/bannedAt from the membership.
type UserResponse struct {
	ID               int64      `json:"id"`
	Email            string     `json:"email"`
	Name             string     `json:"name"`
	Role             string     `json:"role"`
	MembershipStatus string     `json:"membershipStatus" enums:"active,suspended,removed"`
	EmailVerified    bool       `json:"emailVerified"`
	HasPassword      bool       `json:"hasPassword"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	JoinedAt         time.Time  `json:"joinedAt"`
	DeletedAt        *time.Time `json:"deletedAt,omitempty"`
	BannedAt         *time.Time `json:"bannedAt,omitempty"`
}

type FileResponse struct {
	ID           int64      `json:"id"`
	UserID       int64      `json:"userId,omitempty"`
	OriginalName string     `json:"originalName"`
	MimeType     string     `json:"mimeType"`
	Size         int64      `json:"size"`
	URL          string     `json:"url"`
	CreatedAt    time.Time  `json:"createdAt"`
	DeletedAt    *time.Time `json:"deletedAt,omitempty"`
}
