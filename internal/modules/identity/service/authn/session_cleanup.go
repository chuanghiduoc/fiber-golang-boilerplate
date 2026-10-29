package authn

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// sessionCleanupTimeout bounds the post-commit bookkeeping statement. The
// statement is not part of the business transaction, but it must not outlive
// the request or the shutdown drain.
const sessionCleanupTimeout = 3 * time.Second

// cleanupStaleSessions records the revocation of every session a token-version
// bump has already invalidated, in every tenant. It is a follow-up statement
// after the business transaction has committed, never part of it: the version
// bump alone makes every older access and refresh token fail, so a cleanup that
// fails (or a process that dies before it runs) leaves only stale rows, never a
// usable credential. Running it as one bypass statement keeps the change itself
// tenant-scoped and atomic.
func cleanupStaleSessions(ctx context.Context, sessions SessionRepository, userID uuid.UUID, why RevokeReason) {
	if sessions == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
	defer cancel()
	if err := sessions.RevokeStaleForUser(cleanupCtx, userID, why); err != nil {
		slog.Error("failed to record the revocation of invalidated sessions",
			slog.String("user_id", userID.String()),
			slog.String("reason", string(why)),
			slog.Any("error", err),
		)
	}
}

// cleanupOtherSessions is the MFA-disable follow-up: disabling a factor does
// not bump the token version (the caller's own session must keep working), so
// the user's sessions in every tenant except keepID are revoked by one bypass
// statement after the change commits. It is idempotent.
func cleanupOtherSessions(ctx context.Context, sessions SessionRepository, userID, keepID uuid.UUID, why RevokeReason) {
	if err := revokeOtherSessions(ctx, sessions, userID, keepID, why); err != nil {
		slog.Error("failed to record the revocation of other sessions",
			slog.String("user_id", userID.String()),
			slog.String("reason", string(why)),
			slog.Any("error", err),
		)
	}
}

// revokeOtherSessions is the fail-closed form used before a security change
// that removes a factor but keeps the caller's session. A caller that cannot
// finish this cross-tenant revocation must not commit the change.
func revokeOtherSessions(ctx context.Context, sessions SessionRepository, userID, keepID uuid.UUID, why RevokeReason) error {
	if sessions == nil {
		return fmt.Errorf("session repository is not configured")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionCleanupTimeout)
	defer cancel()
	if err := sessions.RevokeOtherUserSessions(cleanupCtx, userID, keepID, why); err != nil {
		return fmt.Errorf("revoke other sessions: %w", err)
	}
	return nil
}
