package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/identity/repository"
)

const (
	// TokenPurgeJobKind is the durable job that deletes expired refresh,
	// password-reset, and email-verification tokens. The scheduler enqueues
	// it every TokenPurgeInterval (SCHEDULER_ENABLED must be true).
	TokenPurgeJobKind = "identity.tokens.purge_expired"
	// TokenPurgeInterval is how often expired tokens are purged. Expired
	// tokens are already rejected on use, so the cadence only bounds growth.
	TokenPurgeInterval = time.Hour

	// tokenPurgeBatchSize bounds one DELETE so a large backlog never holds a
	// long transaction or many row locks.
	tokenPurgeBatchSize = 1000
	// tokenPurgeTableBudget bounds the time one run spends on one table, so a
	// huge refresh-token backlog cannot starve the other tables. Whatever is
	// left is purged by the next run.
	tokenPurgeTableBudget = time.Minute
)

// TokenPurger runs the expired-token retention job.
type TokenPurger struct {
	repo repository.TokenPurgeRepository
}

func NewTokenPurger(repo repository.TokenPurgeRepository) *TokenPurger {
	return &TokenPurger{repo: repo}
}

// HandleJob purges every token table in bounded batches. It is idempotent:
// an at-least-once redelivery only finds fewer rows. The scheduler payload is
// not needed and is ignored.
func (p *TokenPurger) HandleJob(ctx context.Context, _ json.RawMessage) error {
	tables := []struct {
		name  string
		purge func(context.Context, int32) (int64, error)
	}{
		{"refresh_tokens", p.repo.PurgeExpiredRefreshTokens},
		{"password_reset_tokens", p.repo.PurgeExpiredPasswordResetTokens},
		{"email_verification_tokens", p.repo.PurgeExpiredEmailVerificationTokens},
	}
	for _, table := range tables {
		if err := purgeTable(ctx, table.name, table.purge); err != nil {
			return err
		}
	}
	return nil
}

// purgeTable deletes batches until one comes back short (backlog drained) or
// the per-table budget ends. Running out of budget is not a failure: the job
// succeeds and the next run continues. Cancellation of the job itself (for
// example shutdown) is returned so the job is retried.
func purgeTable(ctx context.Context, table string, purge func(context.Context, int32) (int64, error)) error {
	budgetCtx, cancel := context.WithTimeout(ctx, tokenPurgeTableBudget)
	defer cancel()

	var total int64
	defer func() {
		if total > 0 {
			slog.Info("purged expired identity tokens", slog.String("table", table), slog.Int64("rows", total))
		}
	}()
	for {
		deleted, err := purge(budgetCtx, tokenPurgeBatchSize)
		total += deleted
		switch {
		case err == nil && deleted < tokenPurgeBatchSize:
			return nil
		case err == nil:
			continue
		case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
			slog.Warn("expired-token purge budget exhausted; the next run continues", slog.String("table", table))
			return nil
		default:
			return fmt.Errorf("purge expired %s: %w", table, err)
		}
	}
}
