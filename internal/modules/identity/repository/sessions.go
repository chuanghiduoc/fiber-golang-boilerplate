package repository

import (
	"context"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/database"
	identityservice "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/service"
	identitysqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/pagination"
	"github.com/google/uuid"
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
		MFAVerifiedAt: database.TimePtr(row.MfaVerifiedAt),
		LastUsedAt:    row.LastUsedAt.Time,
		CreatedAt:     row.CreatedAt.Time,
		ExpiresAt:     row.ExpiresAt.Time,
		RevokedAt:     database.TimePtr(row.RevokedAt),
		RevokedReason: identityservice.RevokeReason(row.RevokedReason.String),
	}
}

func (r *SessionRepository) Create(ctx context.Context, session identityservice.NewSession) (*identityservice.Session, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.CreateSession(ctx, identitysqlc.CreateSessionParams{
		TenantID:      tenantID,
		UserID:        session.UserID,
		TokenVersion:  session.TokenVersion,
		ExpiresAt:     database.Timestamptz(session.ExpiresAt),
		UserAgent:     optionalText(session.UserAgent),
		Ip:            ipAddr(session.IP),
		Amr:           session.AMR,
		MfaVerifiedAt: database.TimestamptzPtr(session.MFAVerifiedAt),
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	s := toSession(row)
	return &s, nil
}

func (r *SessionRepository) Get(ctx context.Context, id uuid.UUID) (*identityservice.Session, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.GetSession(ctx, identitysqlc.GetSessionParams{TenantID: tenantID, ID: id})
	if err != nil {
		return nil, wrapErr(err)
	}
	s := toSession(row)
	return &s, nil
}

func (r *SessionRepository) ListLiveCursor(ctx context.Context, userID uuid.UUID, cur pagination.DBCursor) ([]identityservice.Session, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := r.q.ListLiveSessionsCursor(ctx, identitysqlc.ListLiveSessionsCursorParams{
		TenantID:        tenantID,
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

func (r *SessionRepository) Revoke(ctx context.Context, userID, id uuid.UUID, why identityservice.RevokeReason) (bool, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return false, err
	}
	n, err := r.q.RevokeSession(ctx, identitysqlc.RevokeSessionParams{
		TenantID: tenantID,
		UserID:   userID,
		ID:       id,
		Reason:   optionalText(string(why)),
	})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *SessionRepository) RevokeAllInTenant(ctx context.Context, userID uuid.UUID, why identityservice.RevokeReason) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	if err := r.q.RevokeTenantUserSessions(ctx, identitysqlc.RevokeTenantUserSessionsParams{
		TenantID: tenantID,
		UserID:   userID,
		Reason:   optionalText(string(why)),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID uuid.UUID, why identityservice.RevokeReason) error {
	ctx = database.WithRLSBypass(ctx) // The user's sessions in every tenant (global account operations).
	if err := r.q.RevokeAllUserSessions(ctx, identitysqlc.RevokeAllUserSessionsParams{
		UserID: userID,
		Reason: optionalText(string(why)),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) RevokeStaleForUser(ctx context.Context, userID uuid.UUID, why identityservice.RevokeReason) error {
	ctx = database.WithRLSBypass(ctx) // The user's stale sessions in every tenant (after-commit cleanup of a version bump).
	if err := r.q.RevokeStaleUserSessions(ctx, identitysqlc.RevokeStaleUserSessionsParams{
		UserID: userID,
		Reason: optionalText(string(why)),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) Restamp(ctx context.Context, userID, id uuid.UUID, tokenVersion int32, amr []string) (*identityservice.Session, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := r.q.RestampSession(ctx, identitysqlc.RestampSessionParams{
		TenantID:     tenantID,
		UserID:       userID,
		ID:           id,
		TokenVersion: tokenVersion,
		Amr:          amr,
	})
	if err != nil {
		return nil, wrapErr(err)
	}
	s := toSession(row)
	return &s, nil
}

func (r *SessionRepository) DeleteRefreshTokens(ctx context.Context, sessionID uuid.UUID) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	if err := r.q.DeleteSessionRefreshTokens(ctx, identitysqlc.DeleteSessionRefreshTokensParams{
		TenantID:  tenantID,
		SessionID: sessionID,
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) RevokeOtherUserSessions(ctx context.Context, userID, keepID uuid.UUID, why identityservice.RevokeReason) error {
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

func (r *SessionRepository) RevokeByRefreshToken(ctx context.Context, tokenHash []byte, why identityservice.RevokeReason) (userID uuid.UUID, revoked bool, err error) {
	ctx = database.WithRLSBypass(ctx) // Possession of the opaque token is the authorization; logout and reuse revoke across the request tenant boundary.
	userID, err = r.q.RevokeSessionByRefreshToken(ctx, identitysqlc.RevokeSessionByRefreshTokenParams{
		TokenHash: tokenHash,
		Reason:    optionalText(string(why)),
	})
	if err != nil {
		if err = wrapErr(err); isNotFound(err) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	return userID, true, nil
}

func (r *SessionRepository) Touch(ctx context.Context, id uuid.UUID, userAgent, ip string) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	if err := r.q.TouchSession(ctx, identitysqlc.TouchSessionParams{
		TenantID:  tenantID,
		ID:        id,
		UserAgent: optionalText(userAgent),
		Ip:        ipAddr(ip),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *SessionRepository) MarkMFAVerified(ctx context.Context, userID, sessionID uuid.UUID) (time.Time, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return time.Time{}, err
	}
	verifiedAt, err := r.q.MarkSessionMFAVerified(ctx, identitysqlc.MarkSessionMFAVerifiedParams{
		TenantID: tenantID,
		UserID:   userID,
		ID:       sessionID,
	})
	if err != nil {
		return time.Time{}, wrapErr(err)
	}
	return verifiedAt.Time, nil
}

func (r *SessionRepository) MarkMFAVerifiedWithMethod(ctx context.Context, userID, sessionID uuid.UUID, method string) (time.Time, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return time.Time{}, err
	}
	verifiedAt, err := r.q.MarkSessionMFAVerifiedWithMethod(ctx, identitysqlc.MarkSessionMFAVerifiedWithMethodParams{
		Method:   method,
		TenantID: tenantID,
		UserID:   userID,
		ID:       sessionID,
	})
	if err != nil {
		return time.Time{}, wrapErr(err)
	}
	return verifiedAt.Time, nil
}

func (r *SessionRepository) State(ctx context.Context, sessionID, userID uuid.UUID) (*identityservice.SessionState, error) {
	row, err := r.q.GetSessionState(ctx, identitysqlc.GetSessionStateParams{ID: sessionID, UserID: userID})
	if err != nil {
		return nil, wrapErr(err)
	}
	return &identityservice.SessionState{
		TenantID:         row.TenantID,
		Revoked:          row.RevokedAt.Valid,
		ExpiresAt:        row.ExpiresAt.Time,
		MFAVerifiedAt:    row.MfaVerifiedAt.Time,
		AMR:              row.Amr,
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

func (r *RefreshTokenRepository) Create(ctx context.Context, sessionID uuid.UUID, tokenHash []byte, expiresAt time.Time) error {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return err
	}
	if err := r.q.CreateRefreshToken(ctx, identitysqlc.CreateRefreshTokenParams{
		TenantID:  tenantID,
		SessionID: sessionID,
		TokenHash: tokenHash,
		ExpiresAt: database.Timestamptz(expiresAt),
	}); err != nil {
		return wrapErr(err)
	}
	return nil
}

func (r *RefreshTokenRepository) GetByHash(ctx context.Context, tokenHash []byte) (*identityservice.RefreshToken, error) {
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
		UsedAt:    database.TimePtr(row.UsedAt),
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func (r *RefreshTokenRepository) MarkUsed(ctx context.Context, scopedTenantID, id uuid.UUID) (bool, error) {
	n, err := r.q.MarkRefreshTokenUsed(ctx, identitysqlc.MarkRefreshTokenUsedParams{TenantID: scopedTenantID, ID: id})
	if err != nil {
		return false, wrapErr(err)
	}
	return n > 0, nil
}

func (r *RefreshTokenRepository) ClassifyRotation(ctx context.Context, tokenHash []byte, usedAfter time.Time) (identityservice.RefreshRotationState, error) {
	tenantID, err := tenant.Require(ctx)
	if err != nil {
		return identityservice.RefreshRotationState{}, err
	}
	row, err := r.q.ClassifyRefreshTokenRotation(ctx, identitysqlc.ClassifyRefreshTokenRotationParams{
		UsedAfter: database.Timestamptz(usedAfter),
		TokenHash: tokenHash,
		TenantID:  tenantID,
	})
	if err != nil {
		return identityservice.RefreshRotationState{}, wrapErr(err)
	}
	return identityservice.RefreshRotationState{
		SessionLive:     row.SessionLive.Bool,
		UsedWithinGrace: row.Recent.Bool,
		Rotated:         row.Rotated,
	}, nil
}
