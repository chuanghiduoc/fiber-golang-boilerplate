package dto

import "time"

const (
	RoleMember = "member"
	RoleAdmin  = "admin"
	RoleOwner  = "owner"
)

// RoleResponse is a system or tenant role definition.
type RoleResponse struct {
	ID          int64     `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Scope       string    `json:"scope" enums:"tenant,platform"`
	System      bool      `json:"system"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// CreateRoleRequest creates a tenant role.
type CreateRoleRequest struct {
	Key         string   `json:"key" validate:"required,min=2,max=50"`
	Name        string   `json:"name" validate:"required,min=1,max=255,notblank"`
	Permissions []string `json:"permissions"`
}

// UpdateRoleDefinitionRequest changes a tenant role's name and permissions.
type UpdateRoleDefinitionRequest struct {
	Name        *string   `json:"name" validate:"omitempty,min=1,max=255,notblank"`
	Permissions *[]string `json:"permissions"`
}

// ReplaceMemberRolesRequest replaces every role of one membership.
type ReplaceMemberRolesRequest struct {
	Roles []string `json:"roles" validate:"required,max=20,dive,required,max=50"`
}
