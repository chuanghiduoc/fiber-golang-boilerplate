package authn

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/core"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service/internal/fakes"
	"github.com/google/uuid"
)

// sessionsEndedBeforeTouch races a revoke/expiry into the rotation window:
// Refresh loads the session before opening its transaction, so the final
// session UPDATE must re-check liveness instead of relying on the earlier read.
type sessionsEndedBeforeTouch struct {
	fakes.SessionRepository
	db     *fakes.MemDB
	before func(*fakes.Session)
}

func (s *sessionsEndedBeforeTouch) Touch(ctx context.Context, id uuid.UUID, userAgent, ip string) error {
	s.db.Mu.Lock()
	if session := s.db.Sessions[id]; session != nil {
		s.before(session)
	}
	s.db.Mu.Unlock()
	return s.SessionRepository.Touch(ctx, id, userAgent, ip)
}

// sessionsEndingTx installs the racing repository inside the transaction, the
// same stores Refresh uses for MarkUsed, the new refresh token, and Touch.
type sessionsEndingTx struct {
	base   fakes.Tx
	db     *fakes.MemDB
	before func(*fakes.Session)
}

func (t sessionsEndingTx) InTx(ctx context.Context, fn func(context.Context, TxRepos) error) error {
	return t.base.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		tx.Sessions = &sessionsEndedBeforeTouch{
			SessionRepository: tx.Sessions,
			db:                t.db,
			before:            t.before,
		}
		return fn(ctx, tx)
	})
}

func TestSessions_RefreshRejectsSessionEndedBeforeRotation(t *testing.T) {
	tests := []struct {
		name   string
		before func(*fakes.Session)
	}{
		{
			name: "revoked after load",
			before: func(session *fakes.Session) {
				now := time.Now()
				session.RevokedAt = &now
				session.RevokedReason = core.RevokeByUser
			},
		},
		{
			name: "expired after load",
			before: func(session *fakes.Session) {
				session.ExpiresAt = time.Now().Add(-time.Second)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := fakes.New()
			ctx := fakes.TenantCtx()
			user, err := newTestUserService(db).Register(ctx, RegisterInput{
				Email: "rotation-race@example.com", Password: fakes.Passphrase, Name: "Race",
			})
			require.NoError(t, err)

			var signed []AccessClaims
			sessions := newTestSessions(db, &signed)
			pair, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
			require.NoError(t, err)
			sessions.tx = sessionsEndingTx{
				base:   fakes.Tx{DB: db},
				db:     db,
				before: tc.before,
			}

			result, err := sessions.Refresh(ctx, pair.RefreshToken, nil)
			require.Error(t, err)
			assert.Nil(t, result)
			assert.Equal(t, http.StatusUnauthorized, statusOf(err))
			assert.Equal(t, string(identitycatalog.CodeAuthRefreshInvalid), codeOf(err))
			assert.Len(t, signed, 1, "refresh must not sign an access token for a dead session")
		})
	}
}

func TestSessions_ReissueExpiredSessionReturnsNoTokens(t *testing.T) {
	db := fakes.New()
	ctx := fakes.TenantCtx()
	user, err := newTestUserService(db).Register(ctx, RegisterInput{
		Email: "reissue-expired@example.com", Password: fakes.Passphrase, Name: "Expired",
	})
	require.NoError(t, err)

	var signed []AccessClaims
	sessions := newTestSessions(db, &signed)
	pair, err := sessions.Issue(ctx, user.User.ID, user.User.TokenVersion, nil)
	require.NoError(t, err)

	db.Mu.Lock()
	db.Sessions[pair.SessionID].ExpiresAt = time.Now().Add(-time.Second)
	db.Mu.Unlock()

	reissued, err := sessions.ReissueTx(ctx, sessions.stores, user.User.ID, pair.SessionID, core.AMROTP)
	require.NoError(t, err)
	assert.Empty(t, reissued)
	assert.Len(t, signed, 1, "an expired session must not be restamped with fresh tokens")
}
