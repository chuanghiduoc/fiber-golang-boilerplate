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
// rows may now be hard-deleted and the IDs whose blob delete failed. A failed
// row is deferred with a per-row backoff, so the rest of the batch continues.
type TrashPurgeFunc func(ctx context.Context, batch []TrashedFile) (purgeIDs, failedIDs []int64, err error)

// TrashRepository is the purge persistence port. Unlike FileRepository it is
// not tenant-scoped: retention is a cross-tenant maintenance pass, and every
// candidate carries its own tenant and storage path.
type TrashRepository interface {
	// PurgeBatch commits a claim on up to limit expired rows (making them
	// unrestorable), calls purge, hard-deletes the returned IDs, and keeps the
	// claim on failed IDs after recording an attempt and retry time: only the
	// blob delete is retried, never restorability. On error the rows stay
	// claimed and the next run finishes them.
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

	var total, failedTotal int64
	defer func() {
		if total > 0 || failedTotal > 0 {
			slog.Info("purged trashed files",
				slog.Int64("rows", total),
				slog.Int64("failed", failedTotal),
			)
		}
	}()
	for {
		var claimed int
		deleted, err := p.repo.PurgeBatch(budgetCtx, p.retentionDays, trashPurgeBatchSize,
			func(ctx context.Context, batch []TrashedFile) ([]int64, []int64, error) {
				claimed = len(batch)
				ids, failed, err := p.deleteBlobs(ctx, batch)
				failedTotal += int64(len(failed))
				return ids, failed, err
			})
		total += deleted
		switch {
		case err == nil && claimed < trashPurgeBatchSize:
			// A failed row is deferred on its own record, so a short batch (or
			// an empty one) means this run has no more eligible rows.
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
// logged, counted, and returned so the repository can defer that row with a
// retry time while the rest of the batch continues. Cancellation aborts the
// batch before its hard delete.
func (p *TrashPurger) deleteBlobs(ctx context.Context, batch []TrashedFile) (ids, failed []int64, err error) {
	ids = make([]int64, 0, len(batch))
	for _, file := range batch {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := p.storage.Delete(ctx, file.StoragePath); err != nil && !errors.Is(err, storage.ErrNotFound) {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			failed = append(failed, file.ID)
			slog.Error("failed to delete trashed file blob; the row is deferred for a later run",
				slog.Int64("file_id", file.ID), slog.Int64("tenant_id", file.TenantID), slog.Any("error", err))
			continue
		}
		ids = append(ids, file.ID)
	}
	return ids, failed, nil
}
