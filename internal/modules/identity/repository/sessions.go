package repository

import (
	"context"
	"time"

	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
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
		AMR:           row.Amr,
		LastUsedAt:    row.LastUsedAt.Time,
		CreatedAt:     row.CreatedAt.Time,
		ExpiresAt:     row.ExpiresAt.Time,
		RevokedAt:     timePtr(row.RevokedAt),
		RevokedReason: identityservice.RevokeReason(row.RevokedReason.String),
	}
}

func (r *SessionRepository) Create(ctx context.Context, session identityservice.NewSession) (*identityservice.Session, error) {
	row, err := r.q.CreateSession(ctx, identitysqlc.CreateSessionParams{
		TenantID:     tenantID(ctx),
		UserID:       session.UserID,
		TokenVersion: session.TokenVersion,
		ExpiresAt:    timestamptz(session.ExpiresAt),
		UserAgent:    optionalText(session.UserAgent),
		Ip:           ipAddr(session.IP),
		Amr:          session.AMR,
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
	ctx = database.WithRLSBypass(ctx) // The user's sessions in every tenant (global account operations).
	if err := r.q.RevokeAllUserSessions(ctx, identitysqlc.RevokeAllUserSessionsParams{
		UserID: userID,
		Reason: optionalText(string(why)),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) RevokeOtherUserSessions(ctx context.Context, userID, keepID int64, why identityservice.RevokeReason) error {
	ctx = database.WithRLSBypass(ctx) // The user's other sessions in every tenant (MFA change).
	if err := r.q.RevokeOtherUserSessions(ctx, identitysqlc.RevokeOtherUserSessionsParams{
		UserID: userID,
		KeepID: keepID,
		Reason: optionalText(string(why)),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) RevokeByRefreshToken(ctx context.Context, tokenHash string, why identityservice.RevokeReason) (userID int64, revoked bool, err error) {
	ctx = database.WithRLSBypass(ctx) // Possession of the opaque token is the authorization; logout and reuse revoke across the request tenant boundary.
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
		ExpiresAt:        row.ExpiresAt.Time,
		Role:             highestRole(row.RoleKeys),
		Permissions:      row.PermissionKeys,
		Platform:         row.Platform,
		MembershipStatus: identityservice.MembershipStatus(row.MembershipStatus),
		TokenVersion:     row.TokenVersion,
		UserStatus:       identityservice.UserStatus(row.UserStatus),
		TenantStatus:     identityservice.TenantStatus(row.TenantStatus),
	}, nil
}

// highestRole maps role keys to the compatibility role field.
func highestRole(keys []string) string {
	for _, key := range keys {
		if key == identityservice.RoleOwner {
			return identityservice.RoleOwner
		}
	}
	for _, key := range keys {
		if key == identityservice.RoleAdmin {
			return identityservice.RoleAdmin
		}
	}
	return identityservice.RoleMember
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
	// Intentionally tenant-scoped: a refresh token is usable only in the
	// tenant that issued it, and RLS is a second line behind the explicit
	// tenant comparison in SessionService.loadRefreshSession. Unlike logout,
	// this lookup has no cross-tenant possession semantics.
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
