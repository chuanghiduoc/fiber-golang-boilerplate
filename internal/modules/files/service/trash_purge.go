package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/storage"
)

const (
	// TrashPurgeJobKind is the durable job that permanently removes files
	// soft-deleted longer than FILES_TRASH_RETENTION_DAYS. The scheduler
	// enqueues it every TrashPurgeInterval (SCHEDULER_ENABLED must be true).
	TrashPurgeJobKind = "files.trash.purge"
	// TrashPurgeInterval is how often the trash is purged. A trashed file is
	// already invisible to users, so the cadence only bounds storage growth.
	TrashPurgeInterval = time.Hour

	// trashPurgeBatchSize bounds one claimed batch.
	trashPurgeBatchSize = 100
	// trashPurgeBudget bounds one run; whatever is left goes to the next run.
	trashPurgeBudget = time.Minute
)

// TrashedFile is one purge candidate: a soft-deleted row and its blob.
type TrashedFile struct {
	ID          int64
	TenantID    int64
	StoragePath string
}

// TrashPurgeFunc removes the blobs of a claimed batch and returns the IDs whose
// rows may now be hard-deleted. A row whose blob could not be removed is left
// out, so it stays claimed for the next run.
type TrashPurgeFunc func(ctx context.Context, batch []TrashedFile) (purgeIDs []int64, err error)

// TrashRepository is the purge persistence port. Unlike FileRepository it is
// not tenant-scoped: retention is a cross-tenant maintenance pass, and every
// candidate carries its own tenant and storage path.
type TrashRepository interface {
	// PurgeBatch commits a claim on up to limit expired rows (making them
	// unrestorable), calls purge, then hard-deletes the returned IDs. On
	// failure the rows stay claimed and the next run finishes them.
	PurgeBatch(ctx context.Context, retentionDays, limit int32, purge TrashPurgeFunc) (int64, error)
}

// TrashPurger runs the trash retention job.
type TrashPurger struct {
	repo          TrashRepository
	storage       storage.Storage
	retentionDays int32
}

// NewTrashPurger builds the purger. retentionDays is validated by config
// (FILES_TRASH_RETENTION_DAYS, 1..3650).
func NewTrashPurger(repo TrashRepository, store storage.Storage, retentionDays int32) *TrashPurger {
	return &TrashPurger{repo: repo, storage: store, retentionDays: retentionDays}
}

// HandleJob purges expired trash in bounded batches: claim, blob, then row
// (a missing blob counts as deleted). It is idempotent under
// at-least-once delivery and publishes no domain event, so no webhook fires:
// the user-visible deletion already emitted file.deleted. The scheduler
// payload is not needed and is ignored.
func (p *TrashPurger) HandleJob(ctx context.Context, _ json.RawMessage) error {
	budgetCtx, cancel := context.WithTimeout(ctx, trashPurgeBudget)
	defer cancel()

	var total int64
	defer func() {
		if total > 0 {
			slog.Info("purged trashed files", slog.Int64("rows", total))
		}
	}()
	for {
		var claimed, failed int
		deleted, err := p.repo.PurgeBatch(budgetCtx, p.retentionDays, trashPurgeBatchSize,
			func(ctx context.Context, batch []TrashedFile) ([]int64, error) {
				claimed = len(batch)
				ids, failures, err := p.deleteBlobs(ctx, batch)
				failed = failures
				return ids, err
			})
		total += deleted
		switch {
		case err == nil && failed > 0:
			// The rest of the batch committed. Stop instead of looping: the
			// failed rows are the oldest and would be selected again at once.
			return fmt.Errorf("purge trashed files: %d blob deletes failed", failed)
		case err == nil && claimed < trashPurgeBatchSize:
			return nil
		case err == nil:
			continue
		case ctx.Err() == nil && budgetCtx.Err() != nil && errors.Is(err, context.DeadlineExceeded):
			slog.Warn("trash purge budget exhausted; the next run continues")
			return nil
		default:
			return fmt.Errorf("purge trashed files: %w", err)
		}
	}
}

// deleteBlobs removes each candidate's blob. A blob that is already gone is a
// success (a previous attempt deleted it before its row). Other failures are
// logged and the row stays claimed. Cancellation aborts the batch before its
// hard delete.
func (p *TrashPurger) deleteBlobs(ctx context.Context, batch []TrashedFile) (ids []int64, failed int, err error) {
	ids = make([]int64, 0, len(batch))
	for _, file := range batch {
		if err := ctx.Err(); err != nil {
			return nil, failed, err
		}
		if err := p.storage.Delete(ctx, file.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
			if ctx.Err() != nil {
				return nil, failed, ctx.Err()
			}
			failed++
			slog.Error("failed to delete trashed file blob; the row is kept for the next run",
				slog.Int64("file_id", file.ID), slog.Int64("tenant_id", file.TenantID), slog.Any("error", err))
			continue
		}
		ids = append(ids, file.ID)
	}
	return ids, failed, nil
}
