package core

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

// ReauthSessionMaxAge is how recent an interactive sign-in must be to stand in
// for a password when a passwordless (OAuth-only) account re-authenticates a
// sensitive change: MFA enrollment, passkey registration, or account erasure.
// A stolen access token alone is then not enough.
const ReauthSessionMaxAge = 15 * time.Minute

// RecentSessionIsLive reports whether sessionID is a live session of userID in
// the request tenant that began within ReauthSessionMaxAge of now. An unknown,
// foreign, revoked, expired, or stale session is reported as false without an
// error; a failed lookup is returned so the caller can fail closed.
func RecentSessionIsLive(ctx context.Context, stores Stores, userID, sessionID uuid.UUID, now time.Time) (bool, error) {
	sess, err := stores.Sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, apperror.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if sess.UserID != userID || sess.TenantID != tenant.IDFromContext(ctx) ||
		sess.RevokedAt != nil || !sess.ExpiresAt.After(now) ||
		now.Sub(sess.CreatedAt) > ReauthSessionMaxAge {
		return false, nil
	}
	return true, nil
}

// MemberPage trims the extra probe row from a member list.
func MemberPage(members []Member, pageSize int) ([]Member, bool, error) {
	hasMore := len(members) > pageSize
	if hasMore {
		members = members[:pageSize]
	}
	return members, hasMore, nil
}

// UserWriteError maps a failed write: a missing row becomes 404 with
// notFoundDetail, a repository AppError (last owner, concurrent change) is
// kept, and anything else becomes a 500 with internalDetail.
func UserWriteError(err error, notFoundDetail, internalDetail string) error {
	if errors.Is(err, apperror.ErrNotFound) {
		return apperror.NewNotFound(notFoundDetail)
	}
	var appErr *apperror.AppError
	if errors.As(err, &appErr) {
		return err
	}
	return apperror.NewInternal(internalDetail)
}

// InvalidateSessions drops the cached session state after a committed change
// to a user's sessions, memberships, or credentials. The database is already
// authoritative, so a cache failure only delays the effect until the cached
// entry expires; it is logged, not returned.
func InvalidateSessions(ctx context.Context, sessions session.Store, userID uuid.UUID) {
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

// EnsureNotLastOwner returns 409 user.last_admin when removing one owner
// would leave the request tenant without any. It first takes the per-tenant
// owner-change lock (the same advisory key the protect_last_owner trigger
// uses), so concurrent demote/ban/remove requests run one at a time and the
// count below sees the previous one's committed result (READ COMMITTED takes
// a fresh snapshot per statement). The trigger remains the database-level
// guarantee for other writers.
func EnsureNotLastOwner(ctx context.Context, memberships MembershipRepository, detail string) error {
	if err := memberships.LockOwnerChanges(ctx); err != nil {
		return apperror.NewInternal("failed to lock owner changes")
	}
	owners, err := memberships.CountActiveOwners(ctx)
	if err != nil {
		return apperror.NewInternal("failed to count owners")
	}
	if owners <= 1 {
		return apperror.NewCode(identitycatalog.CodeUserLastAdmin, detail)
	}
	return nil
}
