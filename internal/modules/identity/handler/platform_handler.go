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

// TenantAdmin is the tenant-administration surface the platform handler uses.
// *tenancy.TenantService implements it.
type TenantAdmin interface {
	List(ctx context.Context, limit int, startingAfter string) ([]core.Tenant, bool, error)
	Get(ctx context.Context, id uuid.UUID) (*core.Tenant, error)
	Create(ctx context.Context, in core.NewTenant) (*core.Tenant, error)
	Update(ctx context.Context, id uuid.UUID, update core.TenantUpdate) (*core.Tenant, error)
	GetOperational(ctx context.Context, id uuid.UUID) (*core.Tenant, error)
}

// PlatformHandler serves the cross-tenant tenant-administration surface.
type PlatformHandler struct {
	tenants TenantAdmin
}

func NewPlatformHandler(tenants TenantAdmin) *PlatformHandler {
	return &PlatformHandler{tenants: tenants}
}

type listTenantsInput struct {
	Limit         int    `query:"limit" default:"20" doc:"Page size (values outside 1-100 are clamped)."`
	StartingAfter string `query:"startingAfter"`
}

type createTenantInput struct {
	Body dto.CreateTenantRequest
}

type tenantInput struct {
	TenantID string `path:"tenantId" format:"uuid"`
}

type updateTenantInput struct {
	TenantID string          `path:"tenantId" format:"uuid"`
	IfMatch  string          `header:"If-Match"`
	Body     dto.TenantPatch `contentType:"application/merge-patch+json" required:"true"`
}

// ListTenants returns every tenant in any lifecycle state.
func (h *PlatformHandler) ListTenants(ctx context.Context, input *listTenantsInput) (*httpapi.ListOutput[dto.TenantResponse], error) {
	tenants, hasMore, err := h.tenants.List(ctx, paginationLimit(input.Limit), input.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := make([]dto.TenantResponse, len(tenants))
	for i := range tenants {
		out[i] = toTenantResponse(&tenants[i])
	}
	return &httpapi.ListOutput[dto.TenantResponse]{
		Body: pagination.NewList("/api/v1/platform/tenants", out, hasMore,
			pagination.NextCursor(out, func(t dto.TenantResponse) (time.Time, uuid.UUID) { return t.CreatedAt, t.ID })),
	}, nil
}

// CreateTenant creates a tenant and makes the caller its first owner.
func (h *PlatformHandler) CreateTenant(ctx context.Context, input *createTenantInput) (*httpapi.Created[dto.TenantResponse], error) {
	status := core.TenantStatus(input.Body.Status)
	if status == "" {
		status = core.TenantStatusActive
	}
	created, err := h.tenants.Create(ctx, core.NewTenant{
		Slug:        input.Body.Slug,
		Name:        input.Body.Name,
		Status:      status,
		TrialEndsAt: input.Body.TrialEndsAt,
		Locale:      input.Body.Locale,
		Timezone:    input.Body.Timezone,
		OwnerUserID: actorID(ctx),
	})
	if err != nil {
		return nil, err
	}
	return httpapi.NewCreated("/api/v1/platform/tenants/"+created.ID.String(), toTenantResponse(created)), nil
}

// GetTenant returns one tenant in any lifecycle state.
func (h *PlatformHandler) GetTenant(ctx context.Context, input *tenantInput) (*httpapi.ETagOutput[dto.TenantResponse], error) {
	id, err := parseUUID(input.TenantID, "tenantId")
	if err != nil {
		return nil, err
	}
	found, err := h.tenants.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.TenantResponse]{ETag: etagHeader(found.UpdatedAt), Body: toTenantResponse(found)}, nil
}

// UpdateTenant applies an RFC 7396 merge patch to a tenant's name and status.
func (h *PlatformHandler) UpdateTenant(ctx context.Context, input *updateTenantInput) (*httpapi.ETagOutput[dto.TenantResponse], error) {
	id, err := parseUUID(input.TenantID, "tenantId")
	if err != nil {
		return nil, err
	}
	found, err := h.tenants.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := checkETag(input.IfMatch, found.UpdatedAt); err != nil {
		return nil, err
	}
	req := input.Body
	if !req.Name.Present && !req.Status.Present {
		return &httpapi.ETagOutput[dto.TenantResponse]{
			ETag: etagHeader(found.UpdatedAt),
			Body: toTenantResponse(found),
		}, nil
	}
	update := core.TenantUpdate{}
	if input.IfMatch != "" {
		expected := found.UpdatedAt
		update.ExpectedUpdatedAt = &expected
	}
	if req.Name.Present {
		if req.Name.Null {
			return nil, unprocessable("name cannot be cleared")
		}
		value := req.Name.Value
		update.Name = &value
	}
	if req.Status.Present {
		if req.Status.Null {
			return nil, unprocessable("status cannot be cleared")
		}
		value := core.TenantStatus(req.Status.Value)
		update.Status = &value
	}
	updated, err := h.tenants.Update(ctx, id, update)
	if err != nil {
		return nil, err
	}
	return &httpapi.ETagOutput[dto.TenantResponse]{ETag: etagHeader(updated.UpdatedAt), Body: toTenantResponse(updated)}, nil
}

func toTenantResponse(t *core.Tenant) dto.TenantResponse {
	return dto.TenantResponse{
		ID:                  t.ID,
		Slug:                t.Slug,
		Name:                t.Name,
		Status:              string(t.Status),
		StatusChangedAt:     t.StatusChangedAt,
		TrialEndsAt:         t.TrialEndsAt,
		DeletionScheduledAt: t.DeletionScheduledAt,
		CreatedAt:           t.CreatedAt,
		UpdatedAt:           t.UpdatedAt,
	}
}
