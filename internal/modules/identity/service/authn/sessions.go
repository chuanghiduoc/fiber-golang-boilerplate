package authn

import (
	"context"
	"errors"
	"log/slog"
	"time"

	identitycatalog "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/catalog"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/session"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/tenant"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/apperror"
	"github.com/google/uuid"
)

// AccessClaims are what an access token carries for one session.
type AccessClaims struct {
	UserID       uuid.UUID
	TenantID     uuid.UUID
	SessionID    uuid.UUID
	Role         string
	TokenVersion int32
	AMR          []string
}

// AccessTokenSigner mints a signed access token (the composition root binds
// it to pkg/token and the JWT keyring).
type AccessTokenSigner func(claims AccessClaims) (string, error)

// RefreshResult is a rotated token pair and the member it was issued to.
type RefreshResult struct {
	Tokens TokenPair
	Member *Member
}

// Admission is a caller-supplied sign-in rule (for example the
// email_verification flag) applied before a session is issued or rotated.
type Admission func(ctx context.Context, member *Member) error

// SessionService issues, rotates, lists, and revokes sign-in sessions. A
// session is the RFC 9700 refresh-token family: refresh tokens rotate on
// every use, and presenting an already-used token revokes the session.
type SessionService struct {
	stores     Stores
	tx         Transactor
	sign       AccessTokenSigner
	refreshTTL time.Duration
	// sessionLifetime is the absolute lifetime of a session
	// (SESSION_MAX_LIFETIME); refresh tokens never outlive it.
	sessionLifetime time.Duration
	cache           session.Store
}

// NewSessionService wires the session service. cache may be nil.
func NewSessionService(stores Stores, tx Transactor, sign AccessTokenSigner, refreshTTL, sessionLifetime time.Duration, cache session.Store) *SessionService {
	return &SessionService{
		stores: stores, tx: tx, sign: sign, refreshTTL: refreshTTL,
		sessionLifetime: sessionLifetime, cache: cache,
	}
}

// errRefreshReused aborts a rotation transaction whose token was consumed
// by a concurrent request.
var errRefreshReused = errors.New("identity: refresh token already used")

// refreshRotationGrace is the window in which a second request presenting the
// token that a concurrent refresh just rotated gets a dedicated 401 instead of
// revoking the shared session. Two browser tabs refreshing at once are a
// normal client race, not a stolen-token reuse.
const refreshRotationGrace = 10 * time.Second

func refreshInvalid(detail string) error {
	return apperror.NewCode(identitycatalog.CodeAuthRefreshInvalid, detail)
}

func refreshConcurrent() error {
	return apperror.NewCode(identitycatalog.CodeAuthRefreshConcurrent,
		"refresh token was used by a concurrent request; the session is still valid")
}

// Issue opens a session in the request tenant for an authenticated user.
// tokenVersion is the version the caller authenticated at: a credential
// change committed since then makes the issue fail with 401.
func (s *SessionService) Issue(ctx context.Context, userID uuid.UUID, tokenVersion int32, amr []string) (TokenPair, error) {
	var pair TokenPair
	err := s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		issued, err := s.issueTx(ctx, tx.Stores, userID, tokenVersion, amr)
		pair = issued
		return err
	})
	return pair, err
}

// issueTx opens a session through st (normally a transaction's stores).
func (s *SessionService) issueTx(ctx context.Context, st Stores, userID uuid.UUID, tokenVersion int32, amr []string) (TokenPair, error) {
	if len(amr) == 0 {
		amr = []string{AMRPassword}
	}
	member, err := st.Memberships.GetActive(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return TokenPair{}, accountNotActive()
		}
		return TokenPair{}, apperror.NewInternal("failed to load membership")
	}
	sess, err := st.Sessions.Create(ctx, NewSession{
		UserID:       userID,
		TokenVersion: tokenVersion,
		ExpiresAt:    time.Now().Add(s.sessionLifetime),
		UserAgent:    userAgentFromContext(ctx),
		IP:           clientIPFromContext(ctx),
		AMR:          amr,
		// A session opened after a second factor starts its AAL2 clock now;
		// a first-factor session leaves it NULL until POST /me/mfa/step-up.
		MFAVerifiedAt: sessionMFAVerifiedAt(amr),
	})
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return TokenPair{}, accountNotActive()
		}
		return TokenPair{}, apperror.NewInternal("failed to create session")
	}
	refresh, err := s.newRefreshToken(ctx, st.RefreshTokens, sess.ID, sess.ExpiresAt)
	if err != nil {
		return TokenPair{}, err
	}
	access, err := s.signFor(member, sess.ID, amr)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{AccessToken: access, RefreshToken: refresh, SessionID: sess.ID}, nil
}

func (s *SessionService) newRefreshToken(ctx context.Context, tokens RefreshTokenRepository, sessionID uuid.UUID, sessionExpiresAt time.Time) (string, error) {
	plain, hash, err := newOpaqueToken()
	if err != nil {
		return "", apperror.NewInternal("failed to generate refresh token")
	}
	// A rotated token never outlives its session, even when the refresh TTL
	// would reach further (the session's absolute lifetime wins).
	expiresAt := time.Now().Add(s.refreshTTL)
	if sessionExpiresAt.Before(expiresAt) {
		expiresAt = sessionExpiresAt
	}
	if err := tokens.Create(ctx, sessionID, hash, expiresAt); err != nil {
		return "", apperror.NewInternal("failed to store refresh token")
	}
	return plain, nil
}

// sessionMFAVerifiedAt returns the session's AAL2 start time: now when the
// session was opened after a second factor (otp/rc), nil for a first-factor
// login that must step up before an MFA-gated route.
func sessionMFAVerifiedAt(amr []string) *time.Time {
	for _, method := range amr {
		if method == AMROTP || method == AMRRecoveryCode ||
			method == AMRHardwareKey || method == AMRSoftwareKey {
			now := time.Now()
			return &now
		}
	}
	return nil
}

func (s *SessionService) signFor(member *Member, sessionID uuid.UUID, amr []string) (string, error) {
	access, err := s.sign(AccessClaims{
		UserID:       member.User.ID,
		TenantID:     member.Membership.TenantID,
		SessionID:    sessionID,
		TokenVersion: member.User.TokenVersion,
		AMR:          amr,
	})
	if err != nil {
		return "", apperror.NewInternal("failed to generate access token")
	}
	return access, nil
}

// reissueTx keeps the session that performed an MFA change alive across the
// change's token-version bump. It restamps the same session row with the user's
// new token version and the proven method added to amr, replaces the session's
// refresh-token family (a token minted before the change must not refresh into
// the stronger amr), and returns a fresh pair for it. The caller's previous
// access token carries the old version and stops verifying. A missing session,
// or one that belongs to another user, is not an error: standalone callers
// (tests, internal jobs) have no session to keep and simply get no pair.
func (s *SessionService) reissueTx(ctx context.Context, st Stores, userID, sessionID uuid.UUID, method string) (TokenPair, error) {
	current, err := st.Sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return TokenPair{}, nil
		}
		return TokenPair{}, apperror.NewInternal("failed to load session")
	}
	if current.UserID != userID {
		return TokenPair{}, nil
	}
	member, err := st.Memberships.GetActive(ctx, userID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return TokenPair{}, accountNotActive()
		}
		return TokenPair{}, apperror.NewInternal("failed to load membership")
	}
	restamped, err := st.Sessions.Restamp(ctx, userID, sessionID, member.User.TokenVersion, withAMR(current.AMR, method))
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return TokenPair{}, nil
		}
		return TokenPair{}, apperror.NewInternal("failed to re-issue session")
	}
	if err := st.Sessions.DeleteRefreshTokens(ctx, restamped.ID); err != nil {
		return TokenPair{}, apperror.NewInternal("failed to replace session refresh tokens")
	}
	refresh, err := s.newRefreshToken(ctx, st.RefreshTokens, restamped.ID, restamped.ExpiresAt)
	if err != nil {
		return TokenPair{}, err
	}
	access, err := s.signFor(member, restamped.ID, restamped.AMR)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{AccessToken: access, RefreshToken: refresh, SessionID: restamped.ID}, nil
}

// withAMR returns amr plus method, without duplicating one already present.
func withAMR(amr []string, method string) []string {
	for _, value := range amr {
		if value == method {
			return append([]string(nil), amr...)
		}
	}
	return append(append([]string(nil), amr...), method)
}

// Refresh rotates a refresh token. The token must belong to the request
// tenant, be unused and unexpired, and its session live with an active
// member whose token version still matches. A token that was already used
// is reuse (RFC 9700): the whole session is revoked and 401 returned.
// admit runs before anything is consumed, so a recoverable refusal (email
// not yet verified) does not burn the token.
func (s *SessionService) Refresh(ctx context.Context, plain string, admit Admission) (*RefreshResult, error) {
	hash := hashToken(plain)
	sess, err := s.loadRefreshSession(ctx, hash)
	if err != nil {
		return nil, err
	}
	member, err := s.stores.Memberships.GetActive(ctx, sess.session.UserID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return nil, accountNotActive()
		}
		return nil, apperror.NewInternal("failed to load membership")
	}
	// A credential change after the session started (password, email,
	// logout-all) bumps the user's token version and ends the session.
	if member.User.TokenVersion != sess.session.TokenVersion {
		return nil, refreshInvalid("refresh token is no longer valid")
	}
	if admit != nil {
		if err := admit(ctx, member); err != nil {
			return nil, err
		}
	}

	var refresh string
	err = s.tx.InTx(ctx, func(ctx context.Context, tx TxRepos) error {
		used, err := tx.RefreshTokens.MarkUsed(ctx, sess.token.TenantID, sess.token.ID)
		if err != nil {
			return apperror.NewInternal("failed to rotate refresh token")
		}
		if !used {
			return errRefreshReused
		}
		if refresh, err = s.newRefreshToken(ctx, tx.RefreshTokens, sess.session.ID, sess.session.ExpiresAt); err != nil {
			return err
		}
		if err := tx.Sessions.Touch(ctx, sess.session.ID, userAgentFromContext(ctx), clientIPFromContext(ctx)); err != nil {
			return apperror.NewInternal("failed to update session")
		}
		return nil
	})
	if errors.Is(err, errRefreshReused) {
		return nil, s.resolveUsedRefresh(ctx, hash)
	}
	if err != nil {
		return nil, err
	}
	access, err := s.signFor(member, sess.session.ID, sess.session.AMR)
	if err != nil {
		return nil, err
	}
	return &RefreshResult{
		Tokens: TokenPair{AccessToken: access, RefreshToken: refresh, SessionID: sess.session.ID},
		Member: member,
	}, nil
}

type refreshSession struct {
	token   *RefreshToken
	session *Session
}

// loadRefreshSession validates a presented refresh token and its session
// without consuming anything (except revoking the family on reuse).
func (s *SessionService) loadRefreshSession(ctx context.Context, hash []byte) (refreshSession, error) {
	rt, err := s.stores.RefreshTokens.GetByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return refreshSession{}, refreshInvalid("invalid refresh token")
		}
		return refreshSession{}, apperror.NewInternal("failed to verify refresh token")
	}
	// Refresh tokens are scoped to their tenant. A token issued by tenant A
	// can never be replayed against tenant B (or the default tenant when the
	// caller omitted the header). This is checked before the reuse branch,
	// so a cross-tenant request cannot revoke another tenant's session.
	if rt.TenantID != tenant.IDFromContext(ctx) {
		return refreshSession{}, refreshInvalid("invalid refresh token")
	}
	if rt.UsedAt != nil {
		return refreshSession{}, s.resolveUsedRefresh(ctx, hash)
	}
	if !rt.ExpiresAt.After(time.Now()) {
		return refreshSession{}, refreshInvalid("refresh token expired")
	}
	sess, err := s.stores.Sessions.Get(ctx, rt.SessionID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return refreshSession{}, refreshInvalid("invalid refresh token")
		}
		return refreshSession{}, apperror.NewInternal("failed to load session")
	}
	if sess.RevokedAt != nil {
		return refreshSession{}, refreshInvalid("session has been revoked")
	}
	if !sess.ExpiresAt.After(time.Now()) {
		return refreshSession{}, refreshInvalid("session has expired")
	}
	return refreshSession{token: rt, session: sess}, nil
}

// resolveUsedRefresh decides what a repeated refresh token means. The token
// and its session are read in one statement, so a concurrent rotation cannot
// slip between the grace check and a revocation: a token consumed less than
// refreshRotationGrace ago by a rotation of a still-live session is the same
// client racing itself and only gets 401 auth.refresh_concurrent. Anything
// else (an older replay, a revoked/expired session, or a token that was not
// rotated) keeps the RFC 9700 reuse response and revokes the family.
func (s *SessionService) resolveUsedRefresh(ctx context.Context, hash []byte) error {
	state, err := s.stores.RefreshTokens.ClassifyRotation(ctx, hash, time.Now().Add(-refreshRotationGrace))
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return refreshInvalid("invalid refresh token")
		}
		slog.Error("failed to classify refresh-token rotation", slog.Any("error", err))
		return apperror.NewInternal("failed to verify refresh token")
	}
	if state.SessionLive && state.UsedWithinGrace && state.Rotated {
		slog.Info("concurrent refresh inside the rotation grace window; session kept")
		return refreshConcurrent()
	}
	return s.refreshReused(ctx, hash)
}

// refreshReused revokes the session that owns a reused refresh token (the
// legitimate client and the attacker hold tokens of the same family, and the
// server cannot tell which is which) and returns the public 401.
func (s *SessionService) refreshReused(ctx context.Context, hash []byte) error {
	userID, revoked, err := s.stores.Sessions.RevokeByRefreshToken(ctx, hash, RevokeRefreshReuse)
	if err != nil {
		slog.Error("failed to revoke session after refresh-token reuse", slog.Any("error", err))
		return apperror.NewInternal("failed to verify refresh token")
	}
	if revoked {
		slog.Warn("refresh token reuse detected; session revoked", slog.String("user_id", userID.String()))
		s.invalidate(ctx, userID)
	}
	return refreshInvalid("refresh token reuse detected; the session was revoked")
}

// Logout revokes the session of a refresh token by possession. Logout must
// work even when the client omits X-Tenant-Slug; possessing the opaque token
// is sufficient to revoke it and cannot be used for any read. Unknown tokens
// succeed silently.
func (s *SessionService) Logout(ctx context.Context, plain string) error {
	userID, revoked, err := s.stores.Sessions.RevokeByRefreshToken(ctx, hashToken(plain), RevokeLogout)
	if err != nil {
		return apperror.NewInternal("failed to revoke refresh token")
	}
	if revoked {
		s.invalidate(ctx, userID)
	}
	return nil
}

// List returns one cursor page of the user's live sessions in the request
// tenant, newest first.
func (s *SessionService) List(ctx context.Context, userID uuid.UUID, limit int, startingAfter string) (listed []Session, more bool, listErr error) {
	cur, pageSize, err := buildCursor(limit, startingAfter)
	if err != nil {
		return nil, false, err
	}
	sessions, err := s.stores.Sessions.ListLiveCursor(ctx, userID, cur)
	if err != nil {
		return nil, false, apperror.NewInternal("failed to list sessions")
	}
	hasMore := len(sessions) > pageSize
	if hasMore {
		sessions = sessions[:pageSize]
	}
	return sessions, hasMore, nil
}

// Revoke ends one of the user's sessions in the request tenant. Another
// user's session (or one in another tenant) behaves as not found.
func (s *SessionService) Revoke(ctx context.Context, userID, sessionID uuid.UUID) error {
	revoked, err := s.stores.Sessions.Revoke(ctx, userID, sessionID, RevokeByUser)
	if err != nil {
		return apperror.NewInternal("failed to revoke session")
	}
	if !revoked {
		return apperror.NewNotFound("session not found")
	}
	s.invalidate(ctx, userID)
	return nil
}

func (s *SessionService) invalidate(ctx context.Context, userID uuid.UUID) {
	invalidateSessions(ctx, s.cache, userID)
}

// invalidateSessions drops the cached session state after a committed change
// to a user's sessions, memberships, or credentials. The database is already
// authoritative, so a cache failure only delays the effect until the cached
// entry expires; it is logged, not returned.
func invalidateSessions(ctx context.Context, sessions session.Store, userID uuid.UUID) {
	if sessions == nil {
		return
	}
	if err := sessions.Invalidate(ctx, userID); err != nil {
		slog.Warn("failed to invalidate cached session state",
			slog.String("user_id", userID.String()),
			slog.Any("error", err),
		)
	}
}

// ReissueTx re-issues the caller's session tokens inside tx after a factor
// change bumped the user's token version. It is the exported seam used by the
// MFA subpackage, which lives outside this package.
func (s *SessionService) ReissueTx(ctx context.Context, st Stores, userID, sessionID uuid.UUID, method string) (TokenPair, error) {
	return s.reissueTx(ctx, st, userID, sessionID, method)
}

// Invalidate drops the cached session state for userID.
func (s *SessionService) Invalidate(ctx context.Context, userID uuid.UUID) {
	s.invalidate(ctx, userID)
}

// IssueTx opens a session inside tx. It is the exported seam used by the MFA
// and passkey subpackages.
func (s *SessionService) IssueTx(ctx context.Context, st Stores, userID uuid.UUID, tokenVersion int32, amr []string) (TokenPair, error) {
	return s.issueTx(ctx, st, userID, tokenVersion, amr)
}
