package dto

import "time"

type RegisterRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required,password"`
	Name     string `json:"name" validate:"required,min=2"`
}

type LoginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refreshToken" validate:"required"`
}

type UpdateUserRequest struct {
	Name  *string `json:"name" validate:"omitempty,min=2"`
	Email *string `json:"email" validate:"omitempty,email"`
}

type ChangePasswordRequest struct {
	CurrentPassword string `json:"currentPassword" validate:"required"`
	NewPassword     string `json:"newPassword" validate:"required,password"`
}

type ForgotPasswordRequest struct {
	Email string `json:"email" validate:"required,email"`
}

type ResetPasswordRequest struct {
	Token    string `json:"token" validate:"required"`
	Password string `json:"password" validate:"required,password"`
}

type VerifyEmailRequest struct {
	Token string `json:"token" validate:"required"`
}

type ResendVerificationRequest struct {
	Email string `json:"email" validate:"required,email"`
}

type UserResponse struct {
	ID            int64     `json:"id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	EmailVerified bool      `json:"emailVerified"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type LoginResponse struct {
	AccessToken  string       `json:"accessToken"`
	RefreshToken string       `json:"refreshToken"`
	User         UserResponse `json:"user"`
}
