package adapter

import (
	"context"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
)

// SessionStateReader adapts identity sessions to session.StateReader, the
// access-token validator's one lookup per (uncached) request.
type SessionStateReader struct {
	sessions identityservice.SessionRepository
}

var _ session.StateReader = (*SessionStateReader)(nil)

func NewSessionStateReader(sessions identityservice.SessionRepository) *SessionStateReader {
	return &SessionStateReader{sessions: sessions}
}

func (r *SessionStateReader) SessionState(ctx context.Context, sessionID, userID int64) (session.State, error) {
	state, err := r.sessions.State(ctx, sessionID, userID)
	if err != nil {
		return session.State{}, err
	}
	return session.State{
		TenantID:     state.TenantID,
		Role:         state.Role,
		Permissions:  state.Permissions,
		Platform:     state.Platform,
		TokenVersion: state.TokenVersion,
		ExpiresAt:    state.ExpiresAt,
		Active: !state.Revoked &&
			state.MembershipStatus == identityservice.MembershipStatusActive &&
			state.UserStatus == identityservice.UserStatusActive,
		TenantActive: state.TenantStatus.Operational(),
	}, nil
}
