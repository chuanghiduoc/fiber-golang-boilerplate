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

type UserResponse struct {
	ID            int64      `json:"id"`
	Email         string     `json:"email"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	EmailVerified bool       `json:"emailVerified"`
	HasPassword   bool       `json:"hasPassword"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	DeletedAt     *time.Time `json:"deletedAt,omitempty"`
	BannedAt      *time.Time `json:"bannedAt,omitempty"`
	// TokenVersion is internal and must never be serialized to clients.
	TokenVersion int32 `json:"-"`
	// TenantID identifies the owning tenant. It is additive and read-only.
	TenantID int64 `json:"tenantId"`
}

type LoginResponse struct {
	AccessToken  string       `json:"accessToken"`
	RefreshToken string       `json:"refreshToken"`
	User         UserResponse `json:"user"`
}
