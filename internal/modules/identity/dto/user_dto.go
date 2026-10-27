package dto

import (
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

type RegisterRequest struct {
	Email    string `json:"email" required:"true" format:"email" maxLength:"255"`
	Password string `json:"password" required:"true"`
	Name     string `json:"name" required:"true" minLength:"2" maxLength:"255"`
}

type LoginRequest struct {
	Email    string `json:"email" required:"true" format:"email" maxLength:"255"`
	Password string `json:"password" required:"true"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refreshToken" required:"true" maxLength:"256"`
}

// GoogleExchangeRequest redeems the single-use code from the Google
// callback's URL fragment.
type GoogleExchangeRequest struct {
	Code string `json:"code" required:"true" minLength:"32" maxLength:"128"`
}

// ProfilePatch is the RFC 7396 merge patch for the caller's global profile.
type ProfilePatch struct {
	Name            patch.Field[string] `json:"name,omitempty"`
	Email           patch.Field[string] `json:"email,omitempty"`
	CurrentPassword patch.Field[string] `json:"currentPassword,omitempty"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword" required:"true"`
	NewPassword     string `json:"newPassword" required:"true"`
}

type ForgotPasswordRequest struct {
	Email string `json:"email" required:"true" format:"email" maxLength:"255"`
}

type ResetPasswordRequest struct {
	Token    string `json:"token" required:"true" maxLength:"256"`
	Password string `json:"password" required:"true"`
}

type VerifyEmailRequest struct {
	Token string `json:"token" required:"true" maxLength:"256"`
}

type ResendVerificationRequest struct {
	Email string `json:"email" required:"true" format:"email" maxLength:"255"`
}

// UserResponse is a global user as seen from one tenant: identity fields
// come from the user, role/status/joinedAt/bannedAt from the membership.
type UserResponse struct {
	ID    uuid.UUID `json:"id"`
	Email string    `json:"email"`
	Name  string    `json:"name"`
	// Role is the highest system role key (owner, admin, or member).
	Role string `json:"role"`
	// Roles lists every role assigned in TenantID.
	Roles []string `json:"roles"`
	// MembershipStatus is active, suspended (banned), or removed.
	MembershipStatus string     `json:"membershipStatus" enum:"active,suspended,removed"`
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
	TenantID uuid.UUID `json:"tenantId"`
}

// LoginResponse is the token response. When a confirmed second factor exists,
// MFARequired is true and the challenge token replaces the pair.
type LoginResponse struct {
	AccessToken  string       `json:"accessToken,omitempty"`
	RefreshToken string       `json:"refreshToken,omitempty"`
	User         UserResponse `json:"user"`
	// MFARequired is true when the password or Google sign-in succeeded but a
	// second factor is needed. Tokens are empty in that case; the caller
	// completes the sign-in with MFAChallengeToken at
	// POST /api/v1/auth/mfa/verifications.
	MFARequired bool `json:"mfaRequired,omitempty"`
	// MFAChallengeToken is a five-minute, single-use, tenant-bound challenge.
	MFAChallengeToken string `json:"mfaChallengeToken,omitempty"`
}
