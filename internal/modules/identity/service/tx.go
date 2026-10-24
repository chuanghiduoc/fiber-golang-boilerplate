package service

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/platform/audit"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// repos are the identity repositories one unit of work writes through. A
// service fills only the repositories it owns in its pool-backed fallback.
type repos struct {
	users   repository.UserRepository
	refresh repository.RefreshTokenRepository
	resets  repository.PasswordResetRepository
}

// inTx runs fn as one unit of work. With a TxManager every repository is
// bound to one transaction, so fn's writes commit or roll back together.
// Without one (unit tests, minimal wiring) fn runs once against fallback and
// tx is nil; callers that need a real transaction must check tx.
func inTx(ctx context.Context, txManager *database.TxManager, fallback repos, fn func(tx pgx.Tx, r repos) error) error {
	if txManager == nil {
		return fn(nil, fallback)
	}
	return txManager.WithTx(ctx, func(tx pgx.Tx) error {
		return fn(tx, repos{
			users:   repository.NewUserRepository(tx),
			refresh: repository.NewRefreshTokenRepository(tx),
			resets:  repository.NewPasswordResetRepository(tx),
		})
	})
}

// checkAudit maps the result of a Tier 1 audit.RecordInTx call: nil stays
// nil, a failure becomes the public 503 audit-unavailable error. Returning it
// from an inTx callback rolls the audited mutation back. Call sites keep the
// audit.RecordInTx call visible next to the action they record.
func checkAudit(err error) error {
	if err != nil {
		return auditWriteError(err)
	}
	return nil
}

// userEvent is a service audit event about one user account.
func userEvent(ctx context.Context, action string, userID int64, metadata map[string]any) audit.Event {
	event := audit.NewServiceEvent(ctx, action, "user")
	event.ResourceID = strconv.FormatInt(userID, 10)
	event.Metadata = metadata
	return event
}
