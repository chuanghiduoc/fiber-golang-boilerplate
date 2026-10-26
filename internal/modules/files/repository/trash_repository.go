package repository

import (
	"context"

	"github.com/jackc/pgx/v5"

	filesvc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/service"
	filesqlc "github.com/chuanghiduoc/fiber-golang-boilerplate/internal/modules/files/sqlc"
	"github.com/chuanghiduoc/fiber-golang-boilerplate/pkg/database"
)

// TrashRepository implements service.TrashRepository. It is the only files
// repository that does not scope queries to the request tenant: the purge is
// a cross-tenant maintenance pass (see queries/trash.sql).
type TrashRepository struct {
	txManager *database.TxManager
}

var _ filesvc.TrashRepository = (*TrashRepository)(nil)

func NewTrashRepository(txManager *database.TxManager) *TrashRepository {
	return &TrashRepository{txManager: txManager}
}

func (r *TrashRepository) PurgeBatch(ctx context.Context, retentionDays, limit int32, purge filesvc.TrashPurgeFunc) (int64, error) {
	ctx = database.WithRLSBypass(ctx) // Trash retention spans every tenant.
	var rows []filesqlc.ClaimTrashedFilesRow
	err := r.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		rows, err = filesqlc.New(tx).ClaimTrashedFiles(ctx, filesqlc.ClaimTrashedFilesParams{
			RetentionDays: retentionDays,
			BatchSize:     limit,
		})
		return err
	})
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	batch := make([]filesvc.TrashedFile, len(rows))
	for i, row := range rows {
		batch[i] = filesvc.TrashedFile{ID: row.ID, TenantID: row.TenantID, StoragePath: row.StoragePath}
	}
	ids, failed, err := purge(ctx, batch)
	if err != nil {
		return 0, err
	}
	if len(failed) > 0 {
		// Release the claim and record the attempt before the hard delete: a
		// permanently failing blob must not keep newer rows behind it, and the
		// row must become restorable again.
		err = r.txManager.WithTx(ctx, func(tx pgx.Tx) error {
			_, err := filesqlc.New(tx).FailTrashedFiles(ctx, failed)
			return err
		})
		if err != nil {
			return 0, err
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	var deleted int64
	err = r.txManager.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		deleted, err = filesqlc.New(tx).HardDeleteTrashedFiles(ctx, ids)
		return err
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
