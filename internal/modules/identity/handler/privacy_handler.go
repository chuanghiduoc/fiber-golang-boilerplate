package handler

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// PrivacyService is the privacy surface the handlers use.
type PrivacyService interface {
	RequestExport(ctx context.Context, userID int64) (*identityservice.DataExport, error)
	ListExports(ctx context.Context, userID int64, limit int, startingAfter string) ([]identityservice.DataExport, bool, error)
	GetExport(ctx context.Context, userID, id int64) (*identityservice.DataExport, error)
	DownloadExport(ctx context.Context, userID, id int64) (*identityservice.DataExport, io.ReadCloser, error)
	RequestSelfErasure(ctx context.Context, userID int64, password string) error
	RequestAdminErasure(ctx context.Context, userID int64) error
}

// PrivacyHandler serves the caller's data exports and erasure requests.
type PrivacyHandler struct {
	service PrivacyService
}

func NewPrivacyHandler(service PrivacyService) *PrivacyHandler {
	return &PrivacyHandler{service: service}
}

func toDataExportResponse(export *identityservice.DataExport) dto.DataExportResponse {
	resp := dto.DataExportResponse{
		ID:        export.ID,
		TenantID:  export.TenantID,
		UserID:    export.UserID,
		Status:    string(export.Status),
		ExpiresAt: export.ExpiresAt,
		CreatedAt: export.CreatedAt,
		UpdatedAt: export.UpdatedAt,
	}
	if export.Status == identityservice.DataExportReady && export.ExpiresAt != nil && export.ExpiresAt.After(time.Now()) {
		resp.DownloadURL = fmt.Sprintf("/api/v1/me/data-exports/%d/download", export.ID)
	}
	return resp
}

func toDataExportResponses(exports []identityservice.DataExport) []dto.DataExportResponse {
	out := make([]dto.DataExportResponse, len(exports))
	for i := range exports {
		out[i] = toDataExportResponse(&exports[i])
	}
	return out
}

// CreateExport godoc
// @Summary Request a data export
// @Description Queue a JSON export of the caller's data in the request tenant. At most one queued or running export exists per membership.
// @Tags Privacy
// @Produce json
// @Security BearerAuth
// @Param Idempotency-Key header string false "Idempotency key"
// @Success 201 {object} dto.DataExportResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "data_export.in_progress"
// @Router /me/data-exports [post]
func (h *PrivacyHandler) CreateExport(c fiber.Ctx) error {
	export, err := h.service.RequestExport(c.Context(), httpx.AuthUserID(c))
	if err != nil {
		return err
	}
	return response.Created(c, toDataExportResponse(export))
}

// ListExports godoc
// @Summary List data exports
// @Description List the caller's export requests in the request tenant, newest first.
// @Tags Privacy
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.DataExportResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Router /me/data-exports [get]
func (h *PrivacyHandler) ListExports(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	exports, hasMore, err := h.service.ListExports(c.Context(), httpx.AuthUserID(c), limit, startingAfter)
	if err != nil {
		return err
	}
	out := toDataExportResponses(exports)
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(e dto.DataExportResponse) (time.Time, int64) { return e.CreatedAt, e.ID }))
}

// GetExport godoc
// @Summary Get a data export
// @Description Status of one of the caller's exports. Another user's or tenant's id is not found.
// @Tags Privacy
// @Produce json
// @Security BearerAuth
// @Param id path int true "Export ID"
// @Success 200 {object} dto.DataExportResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /me/data-exports/{id} [get]
func (h *PrivacyHandler) GetExport(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	export, err := h.service.GetExport(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	return response.Success(c, toDataExportResponse(export))
}

// DownloadExport godoc
// @Summary Download a data export
// @Description Stream the caller's ready JSON export. Other users and tenants get 404; an expired export is 410.
// @Tags Privacy
// @Produce application/json
// @Security BearerAuth
// @Param id path int true "Export ID"
// @Success 200 {file} file
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "data_export.not_ready"
// @Failure 410 {object} apperror.ProblemDetails "data_export.expired"
// @Router /me/data-exports/{id}/download [get]
func (h *PrivacyHandler) DownloadExport(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	export, reader, err := h.service.DownloadExport(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "application/json")
	c.Set(fiber.HeaderContentDisposition, httpx.ContentDispositionAttachment(fmt.Sprintf("data-export-%d.json", export.ID)))
	c.Set(fiber.HeaderCacheControl, "private, no-store")
	return c.SendStream(reader)
}

// EraseMe godoc
// @Summary Erase the caller's account data
// @Description Re-authenticate with the current password, then queue erasure of every tenant membership's data and the global account. Refused while the caller is the last active admin of a tenant.
// @Tags Privacy
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param Idempotency-Key header string false "Idempotency key"
// @Param request body dto.ErasureRequest true "Current password"
// @Success 202 {object} dto.ErasureAccepted
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "user.last_admin"
// @Router /me/erasure [post]
func (h *PrivacyHandler) EraseMe(c fiber.Ctx) error {
	// Erasure ends the global account, which a tenant-scoped API key must not
	// be able to do (same rule as DELETE /users/:id on the caller).
	if authenticatedWithAPIKey(c) {
		return apperror.NewCode(apperror.CodePermissionDenied, "API keys cannot erase an account")
	}
	var req dto.ErasureRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	if err := h.service.RequestSelfErasure(c.Context(), httpx.AuthUserID(c), req.CurrentPassword); err != nil {
		return err
	}
	return response.Accepted(c, dto.ErasureAccepted{Status: "accepted"})
}

// EraseUser godoc
// @Summary Erase a member from the request tenant
// @Description Queue erasure of a member's data and membership in the request tenant only. The global account and the user's memberships in other tenants stay intact. The last active owner of the tenant cannot be erased.
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param Idempotency-Key header string false "Idempotency key"
// @Param id path int true "User ID"
// @Success 202 {object} dto.ErasureAccepted
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails "user.last_admin"
// @Router /admin/users/{id}/erasure [post]
func (h *PrivacyHandler) EraseUser(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.service.RequestAdminErasure(c.Context(), id); err != nil {
		return err
	}
	return response.Accepted(c, dto.ErasureAccepted{Status: "accepted"})
}
