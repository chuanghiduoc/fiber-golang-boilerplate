package handler

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// RoleAdmin is the role surface the role handlers use.
// *membership.RoleService implements it.
type RoleAdmin interface {
	List(ctx context.Context, limit int, startingAfter string) ([]core.Role, bool, error)
	Get(ctx context.Context, id uuid.UUID) (*core.Role, error)
	Create(ctx context.Context, in core.NewRole) (*core.Role, error)
	Update(ctx context.Context, id uuid.UUID, in core.RoleUpdate) (*core.Role, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// RoleHandler serves tenant role management.
type RoleHandler struct {
	roles RoleAdmin
}

func NewRoleHandler(roles RoleAdmin) *RoleHandler {
	return &RoleHandler{roles: roles}
}

type listRolesInput struct {
	Limit         int    `query:"limit" minimum:"1" maximum:"100" default:"20"`
	StartingAfter string `query:"startingAfter"`
}

type createRoleInput struct {
	Body dto.CreateRoleRequest
}

type roleInput struct {
	RoleID string `path:"roleId" format:"uuid"`
}

type updateRoleInput struct {
	RoleID  string `path:"roleId" format:"uuid"`
	IfMatch string `header:"If-Match"`
	RawBody []byte `rawBody:"application/merge-patch+json" contentType:"application/merge-patch+json" required:"true"`
}

// ListRoles returns system roles plus the request tenant's roles.
func (h *RoleHandler) ListRoles(ctx context.Context, input *listRolesInput) (*httpapi.ListOutput[dto.RoleResponse], error) {
	roles, hasMore, err := h.roles.List(ctx, paginationLimit(input.Limit), input.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := make([]dto.RoleResponse, len(roles))
	for i := range roles {
		out[i] = toRoleResponse(&roles[i])
	}
	return &httpapi.ListOutput[dto.RoleResponse]{
		Body: pagination.NewList("/api/v1/roles", out, hasMore,
			pagination.NextCursor(out, func(r dto.RoleResponse) (time.Time, uuid.UUID) { return r.CreatedAt, r.ID })),
	}, nil
}

// CreateRole creates a tenant role.
func (h *RoleHandler) CreateRole(ctx context.Context, input *createRoleInput) (*httpapi.Created[dto.RoleResponse], error) {
	created, err := h.roles.Create(ctx, core.NewRole{
		Key:         input.Body.Key,
		Name:        input.Body.Name,
		Permissions: input.Body.Permissions,
	})
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated("/api/v1/roles/"+created.ID.String(), toRoleResponse(created)), nil
}

// GetRole returns one system or tenant role.
func (h *RoleHandler) GetRole(ctx context.Context, input *roleInput) (*httpapi.ETagOutput[dto.RoleResponse], error) {
	id, err := parseUUID(input.RoleID, "roleId")
	if err != nil {
		return nil, err
	}
	role, err := h.roles.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.RoleResponse]{ETag: etagHeader(role.UpdatedAt), Body: toRoleResponse(role)}, nil
}

// UpdateRole applies an RFC 7396 merge patch to a tenant role.
func (h *RoleHandler) UpdateRole(ctx context.Context, input *updateRoleInput) (*httpapi.ETagOutput[dto.RoleResponse], error) {
	id, err := parseUUID(input.RoleID, "roleId")
	if err != nil {
		return nil, err
	}
	existing, err := h.roles.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkETag(input.IfMatch, existing.UpdatedAt); err != nil {
		return nil, err
	}
	var req dto.RolePatch
	if err := decodeMergePatch(input.RawBody, &req); err != nil {
		return nil, err
	}
	if req.Name.Present && req.Name.Null {
		return nil, unprocessable("name cannot be cleared")
	}
	update := core.RoleUpdate{ID: id}
	if input.IfMatch != "" {
		expected := existing.UpdatedAt
		update.ExpectedUpdatedAt = &expected
	}
	if req.Name.Present {
		value := req.Name.Value
		update.Name = &value
	}
	if req.Permissions.Present {
		// RFC 7396: an explicit null clears the member; a role with no
		// permissions is valid.
		value := req.Permissions.Value
		if req.Permissions.Null {
			value = []string{}
		}
		update.Permissions = &value
	}
	updated, err := h.roles.Update(ctx, id, update)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.RoleResponse]{ETag: etagHeader(updated.UpdatedAt), Body: toRoleResponse(updated)}, nil
}

// DeleteRole deletes an unassigned tenant role.
func (h *RoleHandler) DeleteRole(ctx context.Context, input *roleInput) (*httpapi.Empty, error) {
	id, err := parseUUID(input.RoleID, "roleId")
	if err != nil {
		return nil, err
	}
	if err := h.roles.Delete(ctx, id); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}

func toRoleResponse(role *core.Role) dto.RoleResponse {
	return dto.RoleResponse{
		ID:          role.ID,
		Key:         role.Key,
		Name:        role.Name,
		Scope:       role.Scope,
		System:      role.System(),
		Permissions: role.Permissions,
		CreatedAt:   role.CreatedAt,
		UpdatedAt:   role.UpdatedAt,
	}
}
