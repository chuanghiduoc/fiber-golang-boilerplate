package handler

import (
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"

	apikeydto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/dto"
	apikeysvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/apikeys/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// Handler serves the admin API-key surface.
type Handler struct {
	service apikeysvc.Service
}

func NewHandler(service apikeysvc.Service) *Handler {
	return &Handler{service: service}
}

// Create godoc
// @Summary Create an API key
// @Description Create an API key owned by the authenticated caller. The plaintext key is returned once and never stored.
// @Tags Admin API Keys
// @Accept json
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param request body apikeydto.CreateRequest true "API key creation request"
// @Success 201 {object} apikeydto.KeyWithSecretResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /admin/api-keys [post]
func (h *Handler) Create(c fiber.Ctx) error {
	var req apikeydto.CreateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	key, err := h.service.Create(c.Context(), httpx.AuthUserID(c), toCreateInput(req))
	if err != nil {
		return err
	}
	resp := toCreatedKeyResponse(key)

	setAuditMetadata(c, map[string]any{
		"apiKeyId":    resp.ID,
		"apiKeyName":  resp.Name,
		"ownerUserId": resp.UserID,
		"scopes":      resp.Scopes,
	})
	c.Set(fiber.HeaderLocation, "/api/v1/admin/api-keys/"+strconv.FormatInt(resp.ID, 10))
	return response.Created(c, resp)
}

// List godoc
// @Summary List API keys
// @Description List the request tenant's API keys, including other members', revoked and rotated keys. Secrets are never returned.
// @Tags Admin API Keys
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]apikeydto.APIKeyResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/api-keys [get]
func (h *Handler) List(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	keys, hasMore, err := h.service.List(c.Context(), limit, startingAfter)
	if err != nil {
		return err
	}
	responses := toAPIKeyResponses(keys)
	return response.List(c, responses, hasMore, pagination.NextCursor(responses, func(k apikeydto.APIKeyResponse) (time.Time, int64) { return k.CreatedAt, k.ID }))
}

// Get godoc
// @Summary Get an API key
// @Description Get API-key metadata by id for the request tenant.
// @Tags Admin API Keys
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path int true "API key ID"
// @Success 200 {object} apikeydto.APIKeyResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/api-keys/{id} [get]
func (h *Handler) Get(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	key, err := h.service.Get(c.Context(), id)
	if err != nil {
		return err
	}
	return response.Success(c, toAPIKeyResponse(key))
}

// Update godoc
// @Summary Update an API key
// @Description Update an API key's name and/or scopes. Scopes must remain within the owner's role.
// @Tags Admin API Keys
// @Accept json
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path int true "API key ID"
// @Param request body apikeydto.UpdateRequest true "API key update request"
// @Success 200 {object} apikeydto.APIKeyResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /admin/api-keys/{id} [put]
func (h *Handler) Update(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	var req apikeydto.UpdateRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	key, err := h.service.Update(c.Context(), id, toUpdateInput(req))
	if err != nil {
		return err
	}
	resp := toAPIKeyResponse(key)
	setAuditMetadata(c, map[string]any{
		"apiKeyId":   resp.ID,
		"apiKeyName": resp.Name,
		"scopes":     resp.Scopes,
	})
	return response.Success(c, resp)
}

// Revoke godoc
// @Summary Revoke an API key
// @Description Revoke an API key immediately. Revocation is idempotent.
// @Tags Admin API Keys
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path int true "API key ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/api-keys/{id} [delete]
func (h *Handler) Revoke(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.service.Revoke(c.Context(), id); err != nil {
		return err
	}
	setAuditMetadata(c, map[string]any{"apiKeyId": id})
	return response.NoContent(c)
}

// RevokeByPost godoc
// @Summary Revoke an API key (POST)
// @Description Revoke an API key immediately through an explicit action route.
// @Tags Admin API Keys
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path int true "API key ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/api-keys/{id}/revoke [post]
func (h *Handler) RevokeByPost(c fiber.Ctx) error {
	return h.Revoke(c)
}

// Rotate godoc
// @Summary Rotate an API key
// @Description Issue a replacement key for an API key owned by the authenticated caller. Another member's key id returns 404. The previous key remains valid for the rotation grace window unless revoked.
// @Tags Admin API Keys
// @Produce json
// @Security BearerAuth
// @Security ApiKeyAuth
// @Param id path int true "API key ID"
// @Success 200 {object} apikeydto.RotateResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 409 {object} apperror.ProblemDetails
// @Router /admin/api-keys/{id}/rotate [post]
func (h *Handler) Rotate(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	result, err := h.service.Rotate(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	resp := toRotatedKeyResponse(result)
	setAuditMetadata(c, map[string]any{
		"previousKeyId":         resp.PreviousKeyID,
		"previousKeyValidUntil": resp.PreviousKeyValidUntil,
		"apiKeyId":              resp.ID,
		"apiKeyName":            resp.Name,
		"ownerUserId":           resp.UserID,
		"scopes":                resp.Scopes,
	})
	return response.Success(c, resp)
}

func setAuditMetadata(c fiber.Ctx, metadata map[string]any) {
	fiber.Locals[map[string]any](c, audit.MetadataLocalKey, metadata)
}
