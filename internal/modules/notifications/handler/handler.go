// Package handler owns HTTP transport for the notifications module.
package handler

import (
	"encoding/json"
	"strconv"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/contracts"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/notifications/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// Handler binds requests, validates input, and calls the service.
type Handler struct {
	service service.NotificationService
}

func New(svc service.NotificationService) *Handler {
	return &Handler{service: svc}
}

// List godoc
// @Summary List notifications
// @Description List the authenticated user's notifications in the request tenant
// @Tags Notifications
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Param unreadOnly query bool false "Return only unread notifications"
// @Success 200 {object} response.ListResponse{data=[]dto.NotificationResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /notifications [get]
func (h *Handler) List(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	unreadOnly, _ := strconv.ParseBool(c.Query("unreadOnly"))

	items, hasMore, err := h.service.List(c.Context(), httpx.AuthUserID(c), limit, startingAfter, unreadOnly)
	if err != nil {
		return err
	}
	return response.List(c, items, hasMore, nextCursor(items))
}

// Get godoc
// @Summary Get a notification
// @Description Get one notification owned by the authenticated user
// @Tags Notifications
// @Produce json
// @Security BearerAuth
// @Param id path int true "Notification ID"
// @Success 200 {object} dto.NotificationResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /notifications/{id} [get]
func (h *Handler) Get(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	item, err := h.service.Get(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	return response.Success(c, item)
}

// MarkRead godoc
// @Summary Mark a notification as read
// @Description Mark one notification owned by the authenticated user as read (idempotent)
// @Tags Notifications
// @Produce json
// @Security BearerAuth
// @Param id path int true "Notification ID"
// @Success 200 {object} dto.NotificationResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /notifications/{id}/read [post]
func (h *Handler) MarkRead(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	item, err := h.service.MarkRead(c.Context(), httpx.AuthUserID(c), id)
	if err != nil {
		return err
	}
	return response.Success(c, item)
}

// MarkAllRead godoc
// @Summary Mark all notifications as read
// @Description Mark every unread notification of the authenticated user as read
// @Tags Notifications
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.ReadAllResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /notifications/read-all [post]
func (h *Handler) MarkAllRead(c fiber.Ctx) error {
	updated, err := h.service.MarkAllRead(c.Context(), httpx.AuthUserID(c))
	if err != nil {
		return err
	}
	return response.Success(c, dto.ReadAllResponse{Updated: updated})
}

// UnreadCount godoc
// @Summary Get unread notification count
// @Description Count unread notifications for the authenticated user
// @Tags Notifications
// @Produce json
// @Security BearerAuth
// @Success 200 {object} dto.UnreadCountResponse
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /notifications/unread-count [get]
func (h *Handler) UnreadCount(c fiber.Ctx) error {
	count, err := h.service.UnreadCount(c.Context(), httpx.AuthUserID(c))
	if err != nil {
		return err
	}
	return response.Success(c, dto.UnreadCountResponse{Count: count})
}

// ListPreferences godoc
// @Summary List notification preferences
// @Description List explicit channel preferences for the authenticated user. Missing rows mean the channel is enabled.
// @Tags Notifications
// @Produce json
// @Security BearerAuth
// @Success 200 {object} response.ListResponse{data=[]dto.PreferenceResponse}
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Router /notification-preferences [get]
func (h *Handler) ListPreferences(c fiber.Ctx) error {
	prefs, err := h.service.ListPreferences(c.Context(), httpx.AuthUserID(c))
	if err != nil {
		return err
	}
	return response.List(c, prefs, false)
}

// SetPreference godoc
// @Summary Update a notification preference
// @Description Enable or disable one channel for one notification type
// @Tags Notifications
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.UpdatePreferenceRequest true "Preference"
// @Success 200 {object} dto.PreferenceResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Router /notification-preferences [put]
func (h *Handler) SetPreference(c fiber.Ctx) error {
	var req dto.UpdatePreferenceRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}
	pref, err := h.service.SetPreference(c.Context(), httpx.AuthUserID(c), req)
	if err != nil {
		return err
	}
	return response.Success(c, pref)
}

// Send godoc
// @Summary Send a notification to a user
// @Description Admin-only: create an in-app notification and enqueue its email channel for a user in the request tenant
// @Tags Notifications
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param request body dto.SendNotificationRequest true "Notification"
// @Success 200 {object} dto.SendNotificationResponse
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 403 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Failure 422 {object} apperror.ProblemDetails
// @Failure 500 {object} apperror.ProblemDetails
// @Router /notifications/send [post]
func (h *Handler) Send(c fiber.Ctx) error {
	var req dto.SendNotificationRequest
	if err := httpx.BindAndValidate(c, &req); err != nil {
		return err
	}

	var data json.RawMessage
	if req.Data != nil {
		encoded, err := json.Marshal(req.Data)
		if err != nil {
			return apperror.NewBadRequest("data must be a JSON object")
		}
		data = encoded
	}

	tenantID := tenant.ID(c)
	if tenantID == 0 {
		tenantID = tenant.EffectiveID(c.Context())
	}
	result, err := h.service.Create(c.Context(), contracts.CreateNotificationInput{
		TenantID: tenantID,
		UserID:   req.UserID,
		Type:     req.Type,
		Title:    req.Title,
		Body:     req.Body,
		Data:     data,
	})
	if err != nil {
		return err
	}
	return response.Success(c, dto.SendNotificationResponse{
		Notification: result.Notification,
		EmailQueued:  result.EmailQueued,
	})
}

func nextCursor(items []dto.NotificationResponse) string {
	if len(items) == 0 {
		return ""
	}
	last := items[len(items)-1]
	return pagination.EncodeCursor(last.CreatedAt, last.ID)
}
