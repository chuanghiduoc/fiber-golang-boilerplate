package dto

import (
	"time"

	"github.com/google/uuid"
)

// SessionResponse is one live sign-in session of the caller.
type SessionResponse struct {
	ID         uuid.UUID `json:"id"`
	UserAgent  string    `json:"userAgent,omitempty"`
	IP         string    `json:"ip,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	// Current marks the session of the access token making the request.
	Current bool `json:"current"`
}

// InvitationResponse is a pending (or accepted) invitation. The token is
// never returned; it only travels in the invitation email.
type InvitationResponse struct {
	ID         uuid.UUID   `json:"id"`
	TenantID   uuid.UUID   `json:"tenantId"`
	Email      string      `json:"email"`
	Role       string      `json:"role"`
	RoleIDs    []uuid.UUID `json:"roleIds,omitempty"`
	InvitedBy  uuid.UUID   `json:"invitedBy"`
	ExpiresAt  time.Time   `json:"expiresAt"`
	AcceptedAt *time.Time  `json:"acceptedAt,omitempty"`
	CreatedAt  time.Time   `json:"createdAt"`
}

// AcceptInvitationRequest accepts an invitation as the signed-in user.
type AcceptInvitationRequest struct {
	Token string `json:"token" required:"true" maxLength:"256"`
}

// InvitationSignUpRequest creates an account from an invitation.
type InvitationSignUpRequest struct {
	Token    string `json:"token" required:"true" maxLength:"256"`
	Name     string `json:"name" required:"true" minLength:"2" maxLength:"255"`
	Password string `json:"password" required:"true"`
}

// StatsResponse is the tenant dashboard aggregation identity owns. The file
// counters are served by the files module's own dashboard; identity reports
// only the member counts it owns.
type StatsResponse struct {
	ActiveUsers  int64 `json:"activeUsers"`
	DeletedUsers int64 `json:"deletedUsers"`
}
