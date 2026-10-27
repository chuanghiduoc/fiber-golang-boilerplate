package handler

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// PrivacyService is the privacy surface the handlers use.
type PrivacyService interface {
	RequestExport(ctx context.Context, userID uuid.UUID) (*identityservice.DataExport, error)
	ListExports(ctx context.Context, userID uuid.UUID, limit int, startingAfter string) ([]identityservice.DataExport, bool, error)
	GetExport(ctx context.Context, userID, id uuid.UUID) (*identityservice.DataExport, error)
	DownloadExport(ctx context.Context, userID, id uuid.UUID) (*identityservice.DataExport, io.ReadCloser, error)
	RequestSelfErasure(ctx context.Context, userID uuid.UUID, password string) error
	RequestAdminErasure(ctx context.Context, userID uuid.UUID) error
	ListErasureRequests(ctx context.Context, userID uuid.UUID, limit int, startingAfter string) ([]identityservice.ErasureRequest, bool, error)
	GetErasureRequest(ctx context.Context, userID, id uuid.UUID) (*identityservice.ErasureRequest, error)
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
		resp.ContentURL = fmt.Sprintf("/api/v1/me/data-exports/%s/content", export.ID)
	}
	return resp
}

func toErasureResponse(request *identityservice.ErasureRequest) dto.ErasureRequestResponse {
	return dto.ErasureRequestResponse{
		ID:          request.ID,
		TenantID:    request.TenantID,
		UserID:      request.UserID,
		Scope:       string(request.Scope),
		Status:      string(request.Status),
		RequestedAt: request.RequestedAt,
		CompletedAt: request.CompletedAt,
	}
}

type listExportsInput struct {
	Limit         int    `query:"limit" minimum:"1" maximum:"100" default:"20"`
	StartingAfter string `query:"startingAfter"`
}

type createExportInput struct{}

type exportInput struct {
	ExportID string `path:"exportId" format:"uuid"`
}

type requestErasureInput struct {
	Body dto.ErasureRequest
}

type erasureInput struct {
	RequestID string `path:"requestId" format:"uuid"`
}

type memberErasureInput struct {
	MemberID string `path:"memberId" format:"uuid"`
}

// CreateExport queues a JSON export of the caller's data in the request
// tenant.
func (h *PrivacyHandler) CreateExport(ctx context.Context, _ *createExportInput) (*httpapi.Accepted[dto.DataExportResponse], error) {
	export, err := h.service.RequestExport(ctx, actorID(ctx))
	if err != nil {
		return nil, err
	}
	return httpapi.NewAccepted("/api/v1/me/data-exports/"+export.ID.String(), toDataExportResponse(export)), nil
}

// ListExports lists the caller's export requests.
func (h *PrivacyHandler) ListExports(ctx context.Context, input *listExportsInput) (*httpapi.ListOutput[dto.DataExportResponse], error) {
	exports, hasMore, err := h.service.ListExports(ctx, actorID(ctx), paginationLimit(input.Limit), input.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := make([]dto.DataExportResponse, len(exports))
	for i := range exports {
		out[i] = toDataExportResponse(&exports[i])
	}
	return &httpapi.ListOutput[dto.DataExportResponse]{
		Body: pagination.NewList("/api/v1/me/data-exports", out, hasMore,
			pagination.NextCursor(out, func(e dto.DataExportResponse) (time.Time, uuid.UUID) { return e.CreatedAt, e.ID })),
	}, nil
}

// GetExport returns one of the caller's exports.
func (h *PrivacyHandler) GetExport(ctx context.Context, input *exportInput) (*httpapi.Output[dto.DataExportResponse], error) {
	id, err := parseUUID(input.ExportID, "exportId")
	if err != nil {
		return nil, err
	}
	export, err := h.service.GetExport(ctx, actorID(ctx), id)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.DataExportResponse]{Body: toDataExportResponse(export)}, nil
}

// DownloadExport streams the caller's ready JSON export.
func (h *PrivacyHandler) DownloadExport(ctx context.Context, input *exportInput) (*exportOutput, error) {
	id, err := parseUUID(input.ExportID, "exportId")
	if err != nil {
		return nil, err
	}
	export, reader, err := h.service.DownloadExport(ctx, actorID(ctx), id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, apperror.Internal(err, "failed to read data export")
	}
	return &exportOutput{
		ContentType:        "application/json",
		ContentDisposition: httpx.ContentDispositionAttachment("data-export-" + export.ID.String() + ".json"),
		CacheControl:       "private, no-store",
		Body:               payload,
	}, nil
}

// EraseMe queues erasure of the caller's account data.
func (h *PrivacyHandler) EraseMe(ctx context.Context, input *requestErasureInput) (*httpapi.Accepted[dto.ErasureRequestResponse], error) {
	if authenticatedWithAPIKey(ctx) {
		return nil, apperror.NewCode(apperror.CodePermissionDenied, "API keys cannot erase an account")
	}
	userID := actorID(ctx)
	if err := h.service.RequestSelfErasure(ctx, userID, input.Body.CurrentPassword); err != nil {
		return nil, err
	}
	location := "/api/v1/me/erasure-requests/" + userID.String()
	return httpapi.NewAccepted(location, dto.ErasureRequestResponse{
		ID:          userID,
		TenantID:    tenantIDFromContext(ctx),
		UserID:      userID,
		Scope:       "self",
		Status:      "queued",
		RequestedAt: time.Now().UTC(),
	}), nil
}

// EraseUser queues erasure of a member in the request tenant.
func (h *PrivacyHandler) EraseUser(ctx context.Context, input *memberErasureInput) (*httpapi.Accepted[dto.ErasureRequestResponse], error) {
	id, err := parseUUID(input.MemberID, "memberId")
	if err != nil {
		return nil, err
	}
	if err := h.service.RequestAdminErasure(ctx, id); err != nil {
		return nil, err
	}
	location := "/api/v1/members/" + id.String() + "/erasure-requests/" + id.String()
	return httpapi.NewAccepted(location, dto.ErasureRequestResponse{
		ID:          id,
		TenantID:    tenantIDFromContext(ctx),
		UserID:      id,
		Scope:       "member",
		Status:      "queued",
		RequestedAt: time.Now().UTC(),
	}), nil
}

// ListErasureRequests lists the caller's erasure requests in the request
// tenant.
func (h *PrivacyHandler) ListErasureRequests(ctx context.Context, input *listExportsInput) (*httpapi.ListOutput[dto.ErasureRequestResponse], error) {
	requests, hasMore, err := h.service.ListErasureRequests(ctx, actorID(ctx), paginationLimit(input.Limit), input.StartingAfter)
	if err != nil {
		return nil, err
	}
	out := make([]dto.ErasureRequestResponse, len(requests))
	for i := range requests {
		out[i] = toErasureResponse(&requests[i])
	}
	return &httpapi.ListOutput[dto.ErasureRequestResponse]{
		Body: pagination.NewList("/api/v1/me/erasure-requests", out, hasMore,
			pagination.NextCursor(out, func(r dto.ErasureRequestResponse) (time.Time, uuid.UUID) { return r.RequestedAt, r.ID })),
	}, nil
}

// GetErasureRequest returns one of the caller's erasure requests.
func (h *PrivacyHandler) GetErasureRequest(ctx context.Context, input *erasureInput) (*httpapi.Output[dto.ErasureRequestResponse], error) {
	id, err := parseUUID(input.RequestID, "requestId")
	if err != nil {
		return nil, err
	}
	request, err := h.service.GetErasureRequest(ctx, actorID(ctx), id)
	if err != nil {
		return nil, err
	}
	return &httpapi.Output[dto.ErasureRequestResponse]{Body: toErasureResponse(request)}, nil
}
