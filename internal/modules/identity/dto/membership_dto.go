package dto

import "time"

// SessionResponse is one live sign-in session of the caller.
type SessionResponse struct {
	ID         int64     `json:"id"`
	UserAgent  string    `json:"userAgent,omitempty"`
	IP         string    `json:"ip,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	// Current marks the session of the access token making the request.
	Current bool `json:"current"`
}

// InviteRequest invites an address into the request tenant.
type InviteRequest struct {
	Email string `json:"email" validate:"required,email,max=255"`
	Role  string `json:"role" validate:"omitempty,oneof=user member admin owner"`
}

// InvitationResponse is a pending (or accepted) invitation. The token is
// never returned; it only travels in the invitation email.
type InvitationResponse struct {
	ID         int64      `json:"id"`
	TenantID   int64      `json:"tenantId"`
	Email      string     `json:"email"`
	Role       string     `json:"role"`
	InvitedBy  int64      `json:"invitedBy"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	AcceptedAt *time.Time `json:"acceptedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// AcceptInvitationRequest accepts an invitation as the signed-in user.
type AcceptInvitationRequest struct {
	Token string `json:"token" validate:"required,max=256"`
}

// InvitationSignUpRequest creates an account from an invitation.
type InvitationSignUpRequest struct {
	Token    string `json:"token" validate:"required,max=256"`
	Name     string `json:"name" validate:"required,min=2,max=255,notblank"`
	Password string `json:"password" validate:"required,password"`
}
