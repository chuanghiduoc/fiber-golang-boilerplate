package repository

import (
	"context"
	"time"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
)

// SessionRepository implements service.SessionRepository.
type SessionRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.SessionRepository = (*SessionRepository)(nil)

func NewSessionRepository(db identitysqlc.DBTX) *SessionRepository {
	return &SessionRepository{q: identitysqlc.New(db)}
}

func toSession(row identitysqlc.Session) identityservice.Session {
	return identityservice.Session{
		ID:            row.ID,
		TenantID:      row.TenantID,
		UserID:        row.UserID,
		TokenVersion:  row.TokenVersion,
		UserAgent:     row.UserAgent.String,
		IP:            ipString(row.Ip),
		LastUsedAt:    row.LastUsedAt.Time,
		CreatedAt:     row.CreatedAt.Time,
		RevokedAt:     timePtr(row.RevokedAt),
		RevokedReason: identityservice.RevokeReason(row.RevokedReason.String),
	}
}

func (r *SessionRepository) Create(ctx context.Context, session identityservice.NewSession) (*identityservice.Session, error) {
	row, err := r.q.CreateSession(ctx, identitysqlc.CreateSessionParams{
		TenantID:     tenantID(ctx),
		UserID:       session.UserID,
		TokenVersion: session.TokenVersion,
		UserAgent:    optionalText(session.UserAgent),
		Ip:           ipAddr(session.IP),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	s := toSession(row)
	return &s, nil
}

func (r *SessionRepository) Get(ctx context.Context, id int64) (*identityservice.Session, error) {
	row, err := r.q.GetSession(ctx, identitysqlc.GetSessionParams{TenantID: tenantID(ctx), ID: id})
	if err != nil {
		return nil, wrapErr(err)
	}
	s := toSession(row)
	return &s, nil
}

func (r *SessionRepository) ListLiveCursor(ctx context.Context, userID int64, cur pagination.DBCursor) ([]identityservice.Session, error) {
	rows, err := r.q.ListLiveSessionsCursor(ctx, identitysqlc.ListLiveSessionsCursorParams{
		TenantID:        tenantID(ctx),
		UserID:          userID,
		CursorCreatedAt: cur.CreatedAtParam(),
		CursorID:        cur.IDParam(),
		RowLimit:        cur.Limit,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	sessions := make([]identityservice.Session, len(rows))
	for i := range rows {
		sessions[i] = toSession(rows[i])
	}
	return sessions, nil
}

func (r *SessionRepository) Revoke(ctx context.Context, userID, id int64, why identityservice.RevokeReason) (bool, error) {
	n, err := r.q.RevokeSession(ctx, identitysqlc.RevokeSessionParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
		ID:       id,
		Reason:   optionalText(string(why)),
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *SessionRepository) RevokeAllInTenant(ctx context.Context, userID int64, why identityservice.RevokeReason) error {
	if err := r.q.RevokeTenantUserSessions(ctx, identitysqlc.RevokeTenantUserSessionsParams{
		TenantID: tenantID(ctx),
		UserID:   userID,
		Reason:   optionalText(string(why)),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID int64, why identityservice.RevokeReason) error {
	if err := r.q.RevokeAllUserSessions(ctx, identitysqlc.RevokeAllUserSessionsParams{
		UserID: userID,
		Reason: optionalText(string(why)),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) RevokeByRefreshToken(ctx context.Context, tokenHash string, why identityservice.RevokeReason) (userID int64, revoked bool, err error) {
	userID, err = r.q.RevokeSessionByRefreshToken(ctx, identitysqlc.RevokeSessionByRefreshTokenParams{
		TokenHash: tokenHash,
		Reason:    optionalText(string(why)),
	})
	if err != nil {
		if err = wrapErr(err); isNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return userID, true, nil
}

func (r *SessionRepository) Touch(ctx context.Context, id int64, userAgent, ip string) error {
	if err := r.q.TouchSession(ctx, identitysqlc.TouchSessionParams{
		TenantID:  tenantID(ctx),
		ID:        id,
		UserAgent: optionalText(userAgent),
		Ip:        ipAddr(ip),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) State(ctx context.Context, sessionID, userID int64) (*identityservice.SessionState, error) {
	row, err := r.q.GetSessionState(ctx, identitysqlc.GetSessionStateParams{ID: sessionID, UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &identityservice.SessionState{
		TenantID:         row.TenantID,
		Revoked:          row.RevokedAt.Valid,
		Role:             row.Role,
		MembershipStatus: identityservice.MembershipStatus(row.MembershipStatus),
		TokenVersion:     row.TokenVersion,
		UserStatus:       identityservice.UserStatus(row.UserStatus),
		TenantStatus:     identityservice.TenantStatus(row.TenantStatus),
	}, nil
}

// RefreshTokenRepository implements service.RefreshTokenRepository.
type RefreshTokenRepository struct {
	q *identitysqlc.Queries
}

var _ identityservice.RefreshTokenRepository = (*RefreshTokenRepository)(nil)

func NewRefreshTokenRepository(db identitysqlc.DBTX) *RefreshTokenRepository {
	return &RefreshTokenRepository{q: identitysqlc.New(db)}
}

func (r *RefreshTokenRepository) Create(ctx context.Context, sessionID int64, tokenHash string, expiresAt time.Time) error {
	if err := r.q.CreateRefreshToken(ctx, identitysqlc.CreateRefreshTokenParams{
		TenantID:  tenantID(ctx),
		SessionID: sessionID,
		TokenHash: tokenHash,
		ExpiresAt: timestamptz(expiresAt),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *RefreshTokenRepository) GetByHash(ctx context.Context, tokenHash string) (*identityservice.RefreshToken, error) {
	row, err := r.q.GetRefreshTokenByHash(ctx, tokenHash)
	if err != nil {
		return nil, wrapErr(err)
	}
	return &identityservice.RefreshToken{
		ID:        row.ID,
		TenantID:  row.TenantID,
		SessionID: row.SessionID,
		ExpiresAt: row.ExpiresAt.Time,
		UsedAt:    timePtr(row.UsedAt),
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func (r *RefreshTokenRepository) MarkUsed(ctx context.Context, tenant, id int64) (bool, error) {
	n, err := r.q.MarkRefreshTokenUsed(ctx, identitysqlc.MarkRefreshTokenUsedParams{TenantID: tenant, ID: id})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}
