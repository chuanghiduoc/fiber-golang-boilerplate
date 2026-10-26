package dto

import "time"

type RegisterRequest struct {
	Email    string `json:"email" validate:"required,email,max=255"`
	Password string `json:"password" validate:"required,password"`
	Name     string `json:"name" validate:"required,min=2,max=255,notblank"`
}

type LoginRequest struct {
	Email    string `json:"email" validate:"required,email,max=255"`
	Password string `json:"password" validate:"required"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refreshToken" validate:"required,max=256"`
}

// GoogleExchangeRequest redeems the single-use code from the Google
// callback's URL fragment.
type GoogleExchangeRequest struct {
	Code string `json:"code" validate:"required,min=32,max=128"`
}

type UpdateUserRequest struct {
	Name  *string `json:"name" validate:"omitempty,min=2,max=255,notblank"`
	Email *string `json:"email" validate:"omitempty,email,max=255"`
	// CurrentPassword is required when a non-admin changes their own email.
	CurrentPassword *string `json:"currentPassword,omitempty"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword" validate:"required"`
	NewPassword     string `json:"newPassword" validate:"required,password"`
}

type ForgotPasswordRequest struct {
	Email string `json:"email" validate:"required,email,max=255"`
}

type ResetPasswordRequest struct {
	Token    string `json:"token" validate:"required,max=256"`
	Password string `json:"password" validate:"required,password"`
}

type VerifyEmailRequest struct {
	Token string `json:"token" validate:"required,max=256"`
}

type ResendVerificationRequest struct {
	Email string `json:"email" validate:"required,email,max=255"`
}

// UserResponse is a global user as seen from one tenant: identity fields
// come from the user, role/status/joinedAt/bannedAt from the membership.
type UserResponse struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	// Role is the highest system role key (owner, admin, or member).
	Role string `json:"role"`
	// Roles lists every role assigned in TenantID.
	Roles []string `json:"roles"`
	// MembershipStatus is active, suspended (banned), or removed.
	MembershipStatus string     `json:"membershipStatus" enums:"active,suspended,removed"`
	EmailVerified    bool       `json:"emailVerified"`
	HasPassword      bool       `json:"hasPassword"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	JoinedAt         time.Time  `json:"joinedAt"`
	DeletedAt        *time.Time `json:"deletedAt,omitempty"`
	BannedAt         *time.Time `json:"bannedAt,omitempty"`
	// TokenVersion is internal and must never be serialized to clients.
	TokenVersion int32 `json:"-"`
	// TenantID is the tenant this view (membership) belongs to.
	TenantID int64 `json:"tenantId"`
}

type LoginResponse struct {
	AccessToken  string       `json:"accessToken"`
	RefreshToken string       `json:"refreshToken"`
	User         UserResponse `json:"user"`
	// MFARequired is true when the password or Google sign-in succeeded but a
	// second factor is needed. Tokens are empty in that case; the caller
	// completes the sign-in with MFAChallengeToken at /auth/mfa/verify.
	MFARequired bool `json:"mfaRequired,omitempty"`
	// MFAChallengeToken is a five-minute, single-use, tenant-bound challenge.
	MFAChallengeToken string `json:"mfaChallengeToken,omitempty"`
}
