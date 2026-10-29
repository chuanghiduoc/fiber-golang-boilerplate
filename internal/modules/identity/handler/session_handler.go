package handler

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/dto"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/httpapi"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/token"
)

// SessionHandler lists and revokes the caller's sign-in sessions in the
// request tenant.
type SessionHandler struct {
	sessions SessionIssuer
	keyring  *token.Keyring
}

func NewSessionHandler(sessions SessionIssuer) *SessionHandler {
	return &SessionHandler{sessions: sessions}
}

// SetJWTKeyring installs the keyring used to recover the current session id
// from the access token.
func (h *SessionHandler) SetJWTKeyring(keyring *token.Keyring) { h.keyring = keyring }

type listSessionsInput struct {
	Limit         int    `query:"limit" default:"20" doc:"Page size (values outside 1-100 are clamped)."`
	StartingAfter string `query:"startingAfter"`
	Authorization string `header:"Authorization"`
}

type sessionInput struct {
	SessionID     string `path:"sessionId" format:"uuid"`
	Authorization string `header:"Authorization"`
}

func toSessionResponse(s *core.Session, currentID uuid.UUID) dto.SessionResponse {
	return dto.SessionResponse{
		ID:         s.ID,
		UserAgent:  s.UserAgent,
		IP:         s.IP,
		CreatedAt:  s.CreatedAt,
		LastUsedAt: s.LastUsedAt,
		Current:    s.ID == currentID,
	}
}

// List returns the caller's live sessions in the request tenant.
func (h *SessionHandler) List(ctx context.Context, input *listSessionsInput) (*httpapi.ListOutput[dto.SessionResponse], error) {
	sessions, hasMore, err := h.sessions.List(ctx, actorID(ctx), paginationLimit(input.Limit), input.StartingAfter)
	if err != nil {
		return nil, err
	}
	current := sessionIDFromBearer(h.keyring, input.Authorization)
	out := make([]dto.SessionResponse, len(sessions))
	for i := range sessions {
		out[i] = toSessionResponse(&sessions[i], current)
	}
	return &httpapi.ListOutput[dto.SessionResponse]{
		Body: pagination.NewList("/api/v1/me/sessions", out, hasMore,
			pagination.NextCursor(out, func(s dto.SessionResponse) (time.Time, uuid.UUID) { return s.CreatedAt, s.ID })),
	}, nil
}

// Revoke ends one of the caller's sessions.
func (h *SessionHandler) Revoke(ctx context.Context, input *sessionInput) (*httpapi.Empty, error) {
	id, err := parseUUID(input.SessionID, "sessionId")
	if err != nil {
		return nil, err
	}
	if err := h.sessions.Revoke(ctx, actorID(ctx), id); err != nil {
		return nil, err
	}
	return &httpapi.Empty{}, nil
}
