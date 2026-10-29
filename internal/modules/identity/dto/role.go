package dto

import (
	"time"

	"github.com/google/uuid"
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
	Name        Optional[string]   `json:"name,omitempty" doc:"Replacement role name."`
	Permissions Optional[[]string] `json:"permissions,omitempty" doc:"Replacement permission set; null clears it."`
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
	RoleIDs Optional[RoleIDList] `json:"roleIds,omitempty" doc:"Replacement role ids; null or an empty list is rejected."`
	Status  Optional[string]     `json:"status,omitempty" enum:"active,suspended,removed" doc:"Membership status."`
}

// TenantPatch is the RFC 7396 merge patch for a platform tenant.
type TenantPatch struct {
	Name   Optional[string] `json:"name,omitempty" doc:"Replacement tenant name."`
	Status Optional[string] `json:"status,omitempty" doc:"Replacement tenant status."`
}
