package handler

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// PlatformHandler serves the cross-tenant tenant-administration surface.
type PlatformHandler struct {
	tenants *service.TenantService
}

func NewPlatformHandler(tenants *service.TenantService) *PlatformHandler {
	return &PlatformHandler{tenants: tenants}
}

// ListTenants godoc
// @Summary List tenants (platform)
// @Description Every tenant in any lifecycle state, newest first (platform permission)
// @Tags Platform
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.TenantResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /platform/tenants [get]
func (h *PlatformHandler) ListTenants(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	tenants, hasMore, err := h.tenants.List(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}
	out := make([]dto.TenantResponse, len(tenants))
	for i := range tenants {
		out[i] = toTenantResponse(&tenants[i])
	}
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(t dto.TenantResponse) (time.Time, int64) {
		return t.CreatedAt, t.ID
	}))
}

// CreateTenant godoc
// @Summary Create a tenant (platform)
// @Description Create a tenant and its settings row (platform permission)
// @Tags Platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.CreateTenantRequest true "Tenant"
// @Success 201 {object} dto.TenantResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /platform/tenants [post]
func (h *PlatformHandler) CreateTenant(c fiber.Ctx) error {
	var req dto.CreateTenantRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	status := service.TenantStatus(req.Status)
	if status == "" {
		status = service.TenantStatusActive
	}
	created, err := h.tenants.Create(c.Context(), service.NewTenant{
		Slug:        req.Slug,
		Name:        req.Name,
		Status:      status,
		TrialEndsAt: req.TrialEndsAt,
		Locale:      req.Locale,
		Timezone:    req.Timezone,
		OwnerUserID: httpx.AuthUserID(c),
	})
	if err != nil {
		return err
	}
	return response.Created(c, toTenantResponse(created))
}

// GetTenant godoc
// @Summary Get a tenant (platform)
// @Description Get any tenant by ID in any lifecycle state (platform permission)
// @Tags Platform
// @Produce json
// @Security BearerAuth
// @Param id path int true "Tenant ID"
// @Success 200 {object} dto.TenantResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /platform/tenants/{id} [get]
func (h *PlatformHandler) GetTenant(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	found, err := h.tenants.Get(c.Context(), id)
	if err != nil {
		return err
	}
	return response.Success(c, toTenantResponse(found))
}

// UpdateTenantStatus godoc
// @Summary Update a tenant's status (platform)
// @Description Move a tenant through its lifecycle state machine (platform permission)
// @Tags Platform
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "Tenant ID"
// @Param request body dto.UpdateTenantStatusRequest true "Status"
// @Success 200 {object} dto.TenantResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /platform/tenants/{id}/status [patch]
func (h *PlatformHandler) UpdateTenantStatus(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	var req dto.UpdateTenantStatusRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	updated, err := h.tenants.Transition(c.Context(), id, service.TenantStatus(req.Status))
	if err != nil {
		return err
	}
	return response.Success(c, toTenantResponse(updated))
}

func toTenantResponse(t *service.Tenant) dto.TenantResponse {
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
