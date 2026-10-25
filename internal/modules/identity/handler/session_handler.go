package handler

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpx"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/middleware"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/response"
)

// SessionHandler lists and revokes the caller's sign-in sessions in the
// request tenant. Revoking every session in every tenant is
// POST /auth/logout-all.
type SessionHandler struct {
	sessions SessionIssuer
}

func NewSessionHandler(sessions SessionIssuer) *SessionHandler {
	return &SessionHandler{sessions: sessions}
}

func toSessionResponse(s *service.Session, currentID int64) dto.SessionResponse {
	return dto.SessionResponse{
		ID:         s.ID,
		UserAgent:  s.UserAgent,
		IP:         s.IP,
		CreatedAt:  s.CreatedAt,
		LastUsedAt: s.LastUsedAt,
		Current:    s.ID == currentID,
	}
}

// List godoc
// @Summary List my sessions
// @Description Live sign-in sessions of the authenticated user in the request tenant, newest first. current marks the session of the calling access token.
// @Tags Auth
// @Produce json
// @Security BearerAuth
// @Param limit query int false "Max items to return" default(20)
// @Param startingAfter query string false "Cursor from the last item of the previous page"
// @Success 200 {object} response.ListResponse{data=[]dto.SessionResponse}
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Router /auth/sessions [get]
func (h *SessionHandler) List(c fiber.Ctx) error {
	limit, startingAfter, err := httpx.CursorQuery(c)
	if err != nil {
		return err
	}
	sessions, hasMore, err := h.sessions.List(c.Context(), httpx.AuthUserID(c), limit, startingAfter)
	if err != nil {
		return err
	}
	current := fiber.Locals[int64](c, middleware.LocalSessionID)
	out := make([]dto.SessionResponse, len(sessions))
	for i := range sessions {
		out[i] = toSessionResponse(&sessions[i], current)
	}
	return response.List(c, out, hasMore, pagination.NextCursor(out, func(s dto.SessionResponse) (time.Time, int64) { return s.CreatedAt, s.ID }))
}

// Revoke godoc
// @Summary Revoke one of my sessions
// @Description Ends one live session of the authenticated user in the request tenant (its refresh tokens stop working and its access tokens are rejected). Another user's session behaves as not found.
// @Tags Auth
// @Security BearerAuth
// @Param id path int true "Session ID"
// @Success 204
// @Failure 400 {object} apperror.ProblemDetails
// @Failure 401 {object} apperror.ProblemDetails
// @Failure 404 {object} apperror.ProblemDetails
// @Router /auth/sessions/{id} [delete]
func (h *SessionHandler) Revoke(c fiber.Ctx) error {
	id, err := httpx.ParamID(c, "id")
	if err != nil {
		return err
	}
	if err := h.sessions.Revoke(c.Context(), httpx.AuthUserID(c), id); err != nil {
		return err
	}
	return response.NoContent(c)
}
