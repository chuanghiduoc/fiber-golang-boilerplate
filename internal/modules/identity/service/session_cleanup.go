package service

import (
	"context"
	"log/slog"
)

// cleanupStaleSessions records the revocation of every session a token-version
// bump has already invalidated, in every tenant. It is a follow-up statement
// after the business transaction has committed, never part of it: the version
// bump alone makes every older access and refresh token fail, so a cleanup that
// fails (or a process that dies before it runs) leaves only stale rows, never a
// usable credential. Running it as one bypass statement keeps the change itself
// tenant-scoped and atomic.
func cleanupStaleSessions(ctx context.Context, sessions SessionRepository, userID int64, why RevokeReason) {
	if sessions == nil {
		return
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if err := sessions.RevokeStaleForUser(cleanupCtx, userID, why); err != nil {
		slog.Error("failed to record the revocation of invalidated sessions",
			slog.Int64("user_id", userID),
			slog.String("reason", string(why)),
			slog.Any("error", err),
		)
	}
}

// cleanupOtherSessions is the MFA-disable follow-up: disabling a factor does
// not bump the token version (the caller's own session must keep working), so
// the user's sessions in every tenant except keepID are revoked by one bypass
// statement after the change commits. It is idempotent.
func cleanupOtherSessions(ctx context.Context, sessions SessionRepository, userID, keepID int64, why RevokeReason) {
	if sessions == nil {
		return
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if err := sessions.RevokeOtherUserSessions(cleanupCtx, userID, keepID, why); err != nil {
		slog.Error("failed to record the revocation of other sessions",
			slog.Int64("user_id", userID),
			slog.String("reason", string(why)),
			slog.Any("error", err),
		)
	}
}
