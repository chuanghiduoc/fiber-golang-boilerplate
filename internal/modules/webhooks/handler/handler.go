// Package handler exposes the webhooks HTTP surface. Handlers own only DTO
// validation and status codes; tenant scoping, secrets, dispatch, and retries
// live in the service and repository layers.
package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/dto"
	webhookservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/webhooks/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// Handler serves tenant-scoped webhook administration and the public inbound
// receiver.
type Handler struct {
	service webhookservice.Service
}

// New returns a handler backed by svc.
func New(svc webhookservice.Service) *Handler {
	return &Handler{service: svc}
}

// CreateEndpoint godoc
// @Summary Create a webhook endpoint
// @Description Create a tenant-scoped outbound endpoint. The signing secret is returned once.
// @Tags Webhooks
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.CreateEndpointRequest true "Endpoint configuration"
// @Success 201 {object} dto.EndpointSecretResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /webhooks/endpoints [post]
func (h *Handler) CreateEndpoint(c fiber.Ctx) error {
	var req dto.CreateEndpointRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	result, secret, err := h.service.CreateEndpoint(c.Context(), httpx.AuthUserID(c), createEndpointInput(req))
	if err != nil {
		return err
	}
	return response.Created(c, toEndpointSecretResponse(result, secret))
}

// ListEndpoints godoc
// @Summary List webhook endpoints
// @Description List outbound endpoints for the request tenant.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.EndpointResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /webhooks/endpoints [get]
func (h *Handler) ListEndpoints(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	items, hasMore, err := h.service.ListEndpoints(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}
	out := toEndpointResponses(items)
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(e dto.EndpointResponse) (time.Time, int64) { return e.CreatedAt, e.ID }))
}

// GetEndpoint godoc
// @Summary Get a webhook endpoint
// @Description Get one outbound endpoint by ID.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Endpoint ID"
// @Success 200 {object} dto.EndpointResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/endpoints/{id} [get]
func (h *Handler) GetEndpoint(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	endpoint, err := h.service.GetEndpoint(c.Context(), id)
	if err != nil {
		return err
	}
	return response.Success(c, toEndpointResponse(endpoint))
}

// UpdateEndpoint godoc
// @Summary Update a webhook endpoint
// @Description Replace endpoint URL, filters, status, attempts, and timeout.
// @Tags Webhooks
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "Endpoint ID"
// @Param request body dto.UpdateEndpointRequest true "Endpoint configuration"
// @Success 200 {object} dto.EndpointResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /webhooks/endpoints/{id} [put]
func (h *Handler) UpdateEndpoint(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	var req dto.UpdateEndpointRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	endpoint, err := h.service.UpdateEndpoint(c.Context(), httpx.AuthUserID(c), id, updateEndpointInput(req))
	if err != nil {
		return err
	}
	return response.Success(c, toEndpointResponse(endpoint))
}

// DeleteEndpoint godoc
// @Summary Delete a webhook endpoint
// @Description Soft-delete an endpoint and stop matching it for new events.
// @Tags Webhooks
// @Security BearerAuth
// @Param id path int true "Endpoint ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/endpoints/{id} [delete]
func (h *Handler) DeleteEndpoint(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.service.DeleteEndpoint(c.Context(), httpx.AuthUserID(c), id); err != nil {
		return err
	}
	return response.NoContent(c)
}

// RotateEndpointSecret godoc
// @Summary Rotate a webhook signing secret
// @Description Deactivate the current secret and return a new one once. In-flight deliveries keep their secret snapshot.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Endpoint ID"
// @Param request body dto.RotateEndpointSecretRequest false "Rotation options"
// @Success 200 {object} dto.EndpointSecretResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/endpoints/{id}/rotate-secret [post]
func (h *Handler) RotateEndpointSecret(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	var req dto.RotateEndpointSecretRequest
	if len(c.Body()) > 0 {
		if err := httpx.BindAndValidate(c, &req); err != nil {
			return err
		}
	}
	result, secret, err := h.service.RotateEndpointSecret(c.Context(), httpx.AuthUserID(c), id, req.ExpirePrevious)
	if err != nil {
		return err
	}
	return response.Success(c, toEndpointSecretResponse(result, secret))
}

// ListDeliveries godoc
// @Summary List webhook deliveries
// @Description List delivery history for the request tenant, optionally filtered by endpoint or status.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param endpointId query int false "Endpoint ID"
// @Param status query string false "Delivery status" Enums(pending, succeeded, dead)
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.DeliveryResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /webhooks/deliveries [get]
func (h *Handler) ListDeliveries(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	var endpointID int64
	if raw := c.Query("endpointId"); raw != "" {
		endpointID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || endpointID < 0 {
			return apperror.NewBadRequest("invalid endpointId")
		}
	}
	items, hasMore, err := h.service.ListDeliveries(c.Context(), endpointID, c.Query("status"), limit, startingAfter)
	if err != nil {
		return err
	}
	out := toDeliveryResponses(items)
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(d dto.DeliveryResponse) (time.Time, int64) { return d.CreatedAt, d.ID }))
}

// GetDelivery godoc
// @Summary Get a webhook delivery
// @Description Get one delivery including its retained payload snapshot.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Delivery ID"
// @Success 200 {object} dto.DeliveryResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/deliveries/{id} [get]
func (h *Handler) GetDelivery(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	delivery, err := h.service.GetDelivery(c.Context(), id)
	if err != nil {
		return err
	}
	return response.Success(c, toDeliveryResponse(delivery, true))
}

// ListDeliveryAttempts godoc
// @Summary List delivery attempts
// @Description List every recorded HTTP attempt for one delivery.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Delivery ID"
// @Success 200 {object} response.ListResponse{data=[]dto.DeliveryAttemptResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/deliveries/{id}/attempts [get]
func (h *Handler) ListDeliveryAttempts(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	items, err := h.service.ListDeliveryAttempts(c.Context(), id)
	if err != nil {
		return err
	}
	return response.List(c, toAttemptResponses(items), false)
}

// ReplayDelivery godoc
// @Summary Replay a webhook delivery
// @Description Create a new delivery from a retained payload, using the endpoint's current URL and active secret.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Delivery ID"
// @Success 201 {object} dto.DeliveryResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /webhooks/deliveries/{id}/replay [post]
func (h *Handler) ReplayDelivery(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	delivery, err := h.service.ReplayDelivery(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	return response.Created(c, toDeliveryResponse(delivery, true))
}

// CreateInboundSource godoc
// @Summary Create an inbound webhook source
// @Description Create a tenant-scoped HMAC-authenticated receiver. The secret is returned once.
// @Tags Webhooks
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.CreateInboundSourceRequest true "Inbound source configuration"
// @Success 201 {object} dto.InboundSourceSecretResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /webhooks/inbound-sources [post]
func (h *Handler) CreateInboundSource(c fiber.Ctx) error {
	var req dto.CreateInboundSourceRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	result, secret, err := h.service.CreateInboundSource(c.Context(), httpx.AuthUserID(c), createInboundSourceInput(req))
	if err != nil {
		return err
	}
	return response.Created(c, toInboundSourceSecretResponse(result, secret))
}

// ListInboundSources godoc
// @Summary List inbound webhook sources
// @Description List inbound sources for the request tenant.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.InboundSourceResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /webhooks/inbound-sources [get]
func (h *Handler) ListInboundSources(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	items, hasMore, err := h.service.ListInboundSources(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}
	out := toInboundSourceResponses(items)
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(s dto.InboundSourceResponse) (time.Time, int64) { return s.CreatedAt, s.ID }))
}

// GetInboundSource godoc
// @Summary Get an inbound webhook source
// @Description Get one inbound source by ID.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Source ID"
// @Success 200 {object} dto.InboundSourceResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/inbound-sources/{id} [get]
func (h *Handler) GetInboundSource(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	source, err := h.service.GetInboundSource(c.Context(), id)
	if err != nil {
		return err
	}
	return response.Success(c, toInboundSourceResponse(source))
}

// UpdateInboundSource godoc
// @Summary Update an inbound webhook source
// @Description Replace an inbound source's name, slug, and status.
// @Tags Webhooks
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path int true "Source ID"
// @Param request body dto.UpdateInboundSourceRequest true "Inbound source configuration"
// @Success 200 {object} dto.InboundSourceResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /webhooks/inbound-sources/{id} [put]
func (h *Handler) UpdateInboundSource(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	var req dto.UpdateInboundSourceRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	source, err := h.service.UpdateInboundSource(c.Context(), httpx.AuthUserID(c), id, updateInboundSourceInput(req))
	if err != nil {
		return err
	}
	return response.Success(c, toInboundSourceResponse(source))
}

// DeleteInboundSource godoc
// @Summary Delete an inbound webhook source
// @Description Soft-delete an inbound source and reject future receiver calls.
// @Tags Webhooks
// @Security BearerAuth
// @Param id path int true "Source ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/inbound-sources/{id} [delete]
func (h *Handler) DeleteInboundSource(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.service.DeleteInboundSource(c.Context(), httpx.AuthUserID(c), id); err != nil {
		return err
	}
	return response.NoContent(c)
}

// RotateInboundSourceSecret godoc
// @Summary Rotate an inbound source secret
// @Description Replace the HMAC verification secret and return the new value once.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Source ID"
// @Success 200 {object} dto.InboundSourceSecretResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/inbound-sources/{id}/rotate-secret [post]
func (h *Handler) RotateInboundSourceSecret(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	result, secret, err := h.service.RotateInboundSourceSecret(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	return response.Success(c, toInboundSourceSecretResponse(result, secret))
}

// ListInboundEvents godoc
// @Summary List inbound webhook events
// @Description List deduplicated events received by one inbound source.
// @Tags Webhooks
// @Produce json
// @Security BearerAuth
// @Param id path int true "Source ID"
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.InboundEventResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/inbound-sources/{id}/events [get]
func (h *Handler) ListInboundEvents(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	items, hasMore, err := h.service.ListInboundEvents(c.Context(), id, limit, startingAfter)
	if err != nil {
		return err
	}
	out := toInboundEventResponses(items)
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(e dto.InboundEventResponse) (time.Time, int64) { return e.ReceivedAt, e.ID }))
}

// ReceiveInbound godoc
// @Summary Receive an inbound webhook
// @Description Public Standard Webhooks receiver (HMAC-SHA256 over "id.timestamp.body"). Deduplicates on webhook-id per source; the event type is the body's top-level "type" member ("inbound" when absent).
// @Tags Webhooks
// @Accept application/json
// @Produce json
// @Param id path int true "Inbound source ID"
// @Param webhook-id header string true "Message ID (deduplication key)"
// @Param webhook-timestamp header string true "Unix seconds used in the signature"
// @Param webhook-signature header string true "Space-separated v1,<base64 HMAC-SHA256> signatures"
// @Success 202 {object} dto.InboundReceipt
// @Success 200 {object} dto.InboundReceipt
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /webhooks/inbound/{id} [post]
func (h *Handler) ReceiveInbound(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	headers := http.Header{}
	for key, values := range c.GetReqHeaders() {
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	receipt, err := h.service.ReceiveInbound(c.Context(), id, headers, c.Body())
	if err != nil {
		return err
	}

	// The source, not X-Tenant-Slug, owns the request tenant. Align Fiber
	// locals so the audit middleware records the receiver under the correct
	// tenant and downstream helpers see the same context.
	fiber.Locals[int64](c, "tenant_id", receipt.TenantID)
	c.SetContext(tenant.WithID(c.Context(), receipt.TenantID))

	status := fiber.StatusAccepted
	if receipt.Duplicate {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(toInboundReceipt(receipt))
}
