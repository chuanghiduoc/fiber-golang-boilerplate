package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	// RetentionPurgeJobKind is the durable job that deletes notifications
	// read more than NOTIFICATIONS_RETENTION ago. Unread notifications
	// are kept regardless of age: the user has not seen them yet. The
	// scheduler enqueues it every RetentionPurgeInterval (SCHEDULER_ENABLED
	// must be true).
	RetentionPurgeJobKind = "notifications.retention.purge"
	// RetentionPurgeInterval is how often read notifications are purged.
	RetentionPurgeInterval = time.Hour

	// retentionPurgeBatchSize bounds one DELETE so a backlog never holds a
	// long transaction or many row locks.
	retentionPurgeBatchSize = 1000
	// retentionPurgeBudget bounds one run; the remainder goes to the next run.
	retentionPurgeBudget = time.Minute
)

// RetentionPurger runs the read-notification retention job.
type RetentionPurger struct {
	repo          RetentionRepository
	retentionDays int32
}

// NewRetentionPurger builds the purger. retentionDays is validated by config
// (NOTIFICATIONS_RETENTION, 1..3650).
func NewRetentionPurger(repo RetentionRepository, retentionDays int32) *RetentionPurger {
	return &RetentionPurger{repo: repo, retentionDays: retentionDays}
}

// HandleJob deletes batches until one comes back short (backlog drained) or
// the run budget ends. Running out of budget is not a failure: the next run
// continues. It is idempotent under at-least-once delivery. The scheduler
// payload is not needed and is ignored.
func (p *RetentionPurger) HandleJob(ctx context.Context, _ json.RawMessage) error {
	budgetCtx, cancel := context.WithTimeout(ctx, retentionPurgeBudget)
	defer cancel()

	var total int64
	defer func() {
		if total > 0 {
			slog.Info("purged read notifications", slog.Int64("rows", total))
		}
	}()
	for {
		deleted, err := p.repo.PurgeRead(budgetCtx, p.retentionDays, retentionPurgeBatchSize)
		total += deleted
		switch {
		case err == nil && deleted < retentionPurgeBatchSize:
			return nil
		case err == nil:
			continue
		case ctx.Err() == nil && errors.Is(budgetCtx.Err(), context.DeadlineExceeded):
			slog.Warn("notification retention budget exhausted; the next run continues")
			return nil
		default:
			return fmt.Errorf("purge read notifications: %w", err)
		}
	}
}
