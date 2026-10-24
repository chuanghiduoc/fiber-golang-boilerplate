// Package apikeydto contains the API-key administration wire contract.
// Plaintext credentials appear only in create/rotate responses.
package apikeydto

import "time"

// CreateRequest asks for a new key. UserID defaults to the authenticated
// administrator; when supplied the owner must belong to the request tenant and
// must already hold every requested scope through its role.
type CreateRequest struct {
	Name      string     `json:"name" validate:"required,min=1,max=255"`
	UserID    int64      `json:"userId,omitempty" validate:"omitempty,gt=0"`
	Scopes    []string   `json:"scopes" validate:"required,min=1,max=100,dive,required"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// UpdateRequest changes mutable metadata. Omitted fields are unchanged.
type UpdateRequest struct {
	Name   *string  `json:"name,omitempty" validate:"omitempty,min=1,max=255"`
	Scopes []string `json:"scopes,omitempty" validate:"omitempty,min=1,max=100,dive,required"`
}

// APIKeyResponse is safe to return repeatedly: it contains no hash or secret.
type APIKeyResponse struct {
	ID                 int64      `json:"id"`
	UserID             int64      `json:"userId"`
	Name               string     `json:"name"`
	Prefix             string     `json:"prefix"`
	Scopes             []string   `json:"scopes"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
	RevokedAt          *time.Time `json:"revokedAt,omitempty"`
	RotatedAt          *time.Time `json:"rotatedAt,omitempty"`
	RotationGraceUntil *time.Time `json:"rotationGraceUntil,omitempty"`
	LastUsedAt         *time.Time `json:"lastUsedAt,omitempty"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// KeyWithSecretResponse is returned exactly once on create.
type KeyWithSecretResponse struct {
	APIKeyResponse
	Key string `json:"key"`
}

// RotateResponse returns the new key and the grace window granted to the
// previous key. The previous key remains valid until PreviousKeyValidUntil
// unless it is explicitly revoked.
type RotateResponse struct {
	KeyWithSecretResponse
	PreviousKeyID         int64     `json:"previousKeyId"`
	PreviousKeyValidUntil time.Time `json:"previousKeyValidUntil"`
}
