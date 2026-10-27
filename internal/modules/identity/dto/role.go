package dto

import (
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/patch"
)

const (
	RoleMember = "member"
	RoleAdmin  = "admin"
	RoleOwner  = "owner"
)

// RoleResponse is a system or tenant role definition.
type RoleResponse struct {
	ID          uuid.UUID `json:"id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Scope       string    `json:"scope" enum:"tenant,platform"`
	System      bool      `json:"system"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// CreateRoleRequest creates a tenant role.
type CreateRoleRequest struct {
	Key         string   `json:"key" required:"true" minLength:"2" maxLength:"50"`
	Name        string   `json:"name" required:"true" minLength:"1" maxLength:"255"`
	Permissions []string `json:"permissions"`
}

// RolePatch is the RFC 7396 merge patch for a tenant role.
type RolePatch struct {
	Name        patch.Field[string]   `json:"name,omitempty"`
	Permissions patch.Field[[]string] `json:"permissions,omitempty"`
}

// InviteRequest invites an address into the request tenant.
type InviteRequest struct {
	Email   string      `json:"email" required:"true" format:"email" maxLength:"255"`
	RoleIDs []uuid.UUID `json:"roleIds,omitempty"`
}

// ReplaceMemberRolesRequest replaces every role of one membership.
type ReplaceMemberRolesRequest struct {
	RoleIDs []uuid.UUID `json:"roleIds" required:"true" minItems:"1" maxItems:"20"`
}

// MemberPatch is the RFC 7396 merge patch for a member's roles and status.
type MemberPatch struct {
	RoleIDs patch.Field[[]uuid.UUID] `json:"roleIds,omitempty"`
	Status  patch.Field[string]      `json:"status,omitempty"`
}

// TenantPatch is the RFC 7396 merge patch for a platform tenant.
type TenantPatch struct {
	Name   patch.Field[string] `json:"name,omitempty"`
	Status patch.Field[string] `json:"status,omitempty"`
}
