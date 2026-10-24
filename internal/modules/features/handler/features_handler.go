// Package handler owns HTTP transport for feature-flag administration.
package handler

import (
	"github.com/gofiber/fiber/v3"

	featuresdto "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/dto"
	featuresservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/features/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/features"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// Handler binds requests, validates input, and calls the service.
type Handler struct {
	service featuresservice.Service
}

// NewHandler returns a handler backed by service.
func NewHandler(service featuresservice.Service) *Handler {
	return &Handler{service: service}
}

func requestSubject(c fiber.Ctx) features.Subject {
	return features.SubjectFromContext(c.Context(), httpx.AuthUserID(c))
}

func flagKey(c fiber.Ctx) (string, error) {
	key := c.Params("key")
	if key == "" {
		return "", apperror.NewBadRequest("feature flag key is required")
	}
	return key, nil
}

// ListFlags godoc
// @Summary List feature flags (admin)
// @Description List every compile-time feature flag with its database default, kill switch, and effective value for the caller
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Success 200 {object} response.ListResponse{data=[]featuresdto.FlagResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /admin/features/flags [get]
func (h *Handler) ListFlags(c fiber.Ctx) error {
	flags, err := h.service.ListFlags(c.Context(), requestSubject(c))
	if err != nil {
		return err
	}
	return response.List(c, flags, false)
}

// GetFlag godoc
// @Summary Get a feature flag (admin)
// @Description Get one flag with its database state and every target in the current tenant
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param key path string true "Feature flag key"
// @Success 200 {object} featuresdto.FlagDetailResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/features/flags/{key} [get]
func (h *Handler) GetFlag(c fiber.Ctx) error {
	key, err := flagKey(c)
	if err != nil {
		return err
	}
	flag, err := h.service.GetFlag(c.Context(), key, requestSubject(c))
	if err != nil {
		return err
	}
	return response.Success(c, flag)
}

// UpsertFlag godoc
// @Summary Create or update a feature flag (admin)
// @Description Replace the database default, kill switch, and description for a compile-time flag
// @Tags Admin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param key path string true "Feature flag key"
// @Param request body featuresdto.UpsertFlagRequest true "Flag state"
// @Success 200 {object} featuresdto.FlagResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /admin/features/flags/{key} [put]
func (h *Handler) UpsertFlag(c fiber.Ctx) error {
	key, err := flagKey(c)
	if err != nil {
		return err
	}
	var req featuresdto.UpsertFlagRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	flag, err := h.service.UpsertFlag(c.Context(), key, req, requestSubject(c))
	if err != nil {
		return err
	}

	audit.AddMetadata(c, map[string]any{
		"feature":    flag.Key,
		"enabled":    flag.Enabled,
		"killSwitch": flag.KillSwitch,
		"source":     flag.Source,
	})
	return response.Success(c, flag)
}

// DeleteFlag godoc
// @Summary Delete a feature flag override (admin)
// @Description Delete the database default and all targets, reverting the flag to environment and compiled defaults
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param key path string true "Feature flag key"
// @Success 204
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/features/flags/{key} [delete]
func (h *Handler) DeleteFlag(c fiber.Ctx) error {
	key, err := flagKey(c)
	if err != nil {
		return err
	}
	if err := h.service.DeleteFlag(c.Context(), key); err != nil {
		return err
	}
	audit.AddMetadata(c, map[string]any{"feature": key})
	return response.NoContent(c)
}

// ListTargets godoc
// @Summary List feature flag targets (admin)
// @Description List tenant-wide and user targets for a flag in the current tenant
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param key path string true "Feature flag key"
// @Success 200 {object} response.ListResponse{data=[]featuresdto.TargetResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/features/flags/{key}/targets [get]
func (h *Handler) ListTargets(c fiber.Ctx) error {
	key, err := flagKey(c)
	if err != nil {
		return err
	}
	targets, err := h.service.ListTargets(c.Context(), key)
	if err != nil {
		return err
	}
	return response.List(c, targets, false)
}

// UpsertTarget godoc
// @Summary Create or update a feature flag target (admin)
// @Description Set a tenant-wide target (omit userId) or a user target in the current tenant
// @Tags Admin
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param key path string true "Feature flag key"
// @Param request body featuresdto.UpsertTargetRequest true "Target state"
// @Success 200 {object} featuresdto.TargetResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /admin/features/flags/{key}/targets [put]
func (h *Handler) UpsertTarget(c fiber.Ctx) error {
	key, err := flagKey(c)
	if err != nil {
		return err
	}
	var req featuresdto.UpsertTargetRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	target, err := h.service.UpsertTarget(c.Context(), key, req)
	if err != nil {
		return err
	}

	metadata := map[string]any{
		"feature":  target.FlagKey,
		"targetId": target.ID,
		"enabled":  target.Enabled,
	}
	if target.UserID != nil {
		metadata["userId"] = *target.UserID
	}
	audit.AddMetadata(c, metadata)
	return response.Success(c, target)
}

// DeleteTarget godoc
// @Summary Delete a feature flag target (admin)
// @Description Delete a tenant-wide or user target by ID from the current tenant
// @Tags Admin
// @Produce json
// @Security BearerAuth
// @Param key path string true "Feature flag key"
// @Param id path int true "Target ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /admin/features/flags/{key}/targets/{id} [delete]
func (h *Handler) DeleteTarget(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	key, err := flagKey(c)
	if err != nil {
		return err
	}
	if err := h.service.DeleteTarget(c.Context(), key, id); err != nil {
		return err
	}
	audit.AddMetadata(c, map[string]any{"feature": key, "targetId": id})
	return response.NoContent(c)
}
