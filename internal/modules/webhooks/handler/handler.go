// Package handler exposes the webhooks HTTP surface. Handlers own only DTO
// validation and status codes; tenant scoping, secrets, dispatch, and retries
// live in the service and repository layers.
package handler

import (
	"net/http"
	"strconv"

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
	result, err := h.service.CreateEndpoint(c.Context(), httpx.AuthUserID(c), req)
	if err != nil {
		return err
	}
	return response.Created(c, result)
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
	return response.List(c, items, hasMore, nextEndpointCursor(items))
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
	return response.Success(c, endpoint)
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
	endpoint, err := h.service.UpdateEndpoint(c.Context(), httpx.AuthUserID(c), id, req)
	if err != nil {
		return err
	}
	return response.Success(c, endpoint)
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
	result, err := h.service.RotateEndpointSecret(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	return response.Success(c, result)
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
	return response.List(c, items, hasMore, nextDeliveryCursor(items))
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
	return response.Success(c, delivery)
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
	return response.List(c, items, false)
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
	return response.Created(c, delivery)
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
	result, err := h.service.CreateInboundSource(c.Context(), httpx.AuthUserID(c), req)
	if err != nil {
		return err
	}
	return response.Created(c, result)
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
	return response.List(c, items, hasMore, nextInboundSourceCursor(items))
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
	return response.Success(c, source)
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
	source, err := h.service.UpdateInboundSource(c.Context(), httpx.AuthUserID(c), id, req)
	if err != nil {
		return err
	}
	return response.Success(c, source)
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
	result, err := h.service.RotateInboundSourceSecret(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	return response.Success(c, result)
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
	return response.List(c, items, hasMore, nextInboundEventCursor(items))
}

// ReceiveInbound godoc
// @Summary Receive an inbound webhook
// @Description Public HMAC-authenticated receiver. Deduplicates on X-Webhook-Id per source.
// @Tags Webhooks
// @Accept application/json
// @Produce json
// @Param id path int true "Inbound source ID"
// @Param X-Webhook-Id header string true "External event ID"
// @Param X-Webhook-Event header string false "Event type"
// @Param X-Webhook-Timestamp header string true "Unix seconds used in the signature"
// @Param X-Webhook-Signature header string true "v1=<hex HMAC-SHA256>"
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
	return c.Status(status).JSON(receipt)
}

func nextEndpointCursor(items []dto.EndpointResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}

func nextDeliveryCursor(items []dto.DeliveryResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}

func nextInboundSourceCursor(items []dto.InboundSourceResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}

func nextInboundEventCursor(items []dto.InboundEventResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.ReceivedAt, last.ID)
}
