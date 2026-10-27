package adapter

import (
	"context"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/google/uuid"
)

// SessionStateReader adapts identity sessions to session.StateReader, the
// access-token validator's one lookup per (uncached) request.
type SessionStateReader struct {
	sessions core.SessionRepository
}

var _ session.StateReader = (*SessionStateReader)(nil)

func NewSessionStateReader(sessions core.SessionRepository) *SessionStateReader {
	return &SessionStateReader{sessions: sessions}
}

func (r *SessionStateReader) SessionState(ctx context.Context, sessionID, userID uuid.UUID) (session.State, error) {
	state, err := r.sessions.State(ctx, sessionID, userID)
	if err != nil {
		return session.State{}, err
	}
	return session.State{
		TenantID:      state.TenantID,
		Role:          state.Role,
		Permissions:   state.Permissions,
		Platform:      state.Platform,
		TokenVersion:  state.TokenVersion,
		ExpiresAt:     state.ExpiresAt,
		MFAVerifiedAt: state.MFAVerifiedAt,
		AMR:           state.AMR,
		Active: !state.Revoked &&
			state.MembershipStatus == core.MembershipStatusActive &&
			state.UserStatus == core.UserStatusActive,
		TenantActive: state.TenantStatus.Operational(),
	}, nil
}
